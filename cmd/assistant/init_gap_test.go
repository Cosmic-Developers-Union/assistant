package main

import (
	"net/http"
	"strings"
	"testing"
)

// initFailFake 在 initFake 之上再加三个「服务端说不」的开关。init 的写路径
// （收敛标签、加协作者）此前只被测过成功姿态：所有替身都回 2xx，于是
// 「服务端拒绝时错误怎么包」这条分支从未被走过，而它恰恰是操作者在真实站点上
// 最常撞到的一类失败（令牌权限不够、标签名冲突、组织策略限制）。
//
// failWrite 按端点后缀拦截写操作：路径告诉替身「你要失败的是哪一次调用」，
// 这样同一个替身可以只挡住标签、放过协作者，反之亦然。
type initFailFake struct {
	*initFake
	// labelStatus 非零时，标签端点的写操作（POST/PATCH/DELETE）返回它。
	labelStatus int
	// collaboratorStatus 非零时，协作者写操作（PUT/DELETE）返回它。
	collaboratorStatus int
}

// newInitFailFake 构造带失败开关的替身。
func newInitFailFake(t *testing.T) *initFailFake {
	t.Helper()
	fail := &initFailFake{}
	inner := &initFake{self: "dev", permissions: map[string]string{}}
	fail.initFake = inner
	inner.server = newInitServerWithFailures(t, fail)
	t.Cleanup(inner.server.Close)
	return fail
}

// TestRunInitLabelsReportsReconcileFailure 断言标签收敛失败时错误被包上
// 「收敛标签体系」的语境。
//
// init labels 的承诺是「收敛后标签完全合规，label-sync 无需再修正」；服务端
// 拒绝若被吞成一句无语境的 403，操作者分不清是标签端点的问题还是整站不可达，
// 而后续 label-sync 仍会反复改动同一批标签——诊断方向完全错。
func TestRunInitLabelsReportsReconcileFailure(t *testing.T) {
	fail := newInitFailFake(t)
	fail.labelStatus = http.StatusForbidden
	fail.collaborators = []string{"dev"}
	fail.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fail.initFake)

	fixture := newInitLeafForTest(t, "labels", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil {
		t.Fatalf("标签端点拒绝时应报错：\n%s", fixture.out.String())
	}
	if !strings.Contains(err.Error(), "收敛标签体系") {
		t.Errorf("错误应点明是收敛标签这一步失败的：%v", err)
	}
}

// TestRunInitMergeReportsCollaboratorFailure 断言加协作者失败时错误上报。
//
// init merge 是「让 merge 账号拿仓库 admin 权限」的唯一入口；这个 PUT 静默失败
// 会让分支保护里写下的合并白名单落在一个没有权限的账号上——门禁看起来配好了，
// 实际没有任何人能合并。错误必须当场冒出来。
func TestRunInitMergeReportsCollaboratorFailure(t *testing.T) {
	fail := newInitFailFake(t)
	fail.collaboratorStatus = http.StatusForbidden
	fail.collaborators = []string{"dev"}
	fail.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fail.initFake)

	fixture := newInitLeafForTest(t, "merge", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil {
		t.Fatalf("加协作者失败时应报错：\n%s", fixture.out.String())
	}
	if !strings.Contains(err.Error(), "协作者") {
		t.Errorf("错误应提到协作者：%v", err)
	}
	// 失败时不能声称已经加上了。
	if strings.Contains(fixture.out.String(), "已把 merge 加为") {
		t.Errorf("失败时不该输出成功叙述：\n%s", fixture.out.String())
	}
}

// TestRunInitReviewerReportsCollaboratorFailure 断言同一个 PUT 在 ai 路径上的
// 失败同样上报：ai 加不上就意味着没有内容评审者，双批准的另一半缺失。
func TestRunInitReviewerReportsCollaboratorFailure(t *testing.T) {
	fail := newInitFailFake(t)
	fail.collaboratorStatus = http.StatusUnprocessableEntity
	fail.collaborators = []string{"dev"}
	fail.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fail.initFake)

	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil {
		t.Fatalf("加协作者失败时应报错：\n%s", fixture.out.String())
	}
	if strings.Contains(fixture.out.String(), "已把 ai 加为") {
		t.Errorf("失败时不该输出成功叙述：\n%s", fixture.out.String())
	}
}
