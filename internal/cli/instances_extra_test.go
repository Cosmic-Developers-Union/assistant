package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// giteaFileWithRepos 造一份只含一个 gitea 通道的配置对象：host 必须与凭据库
// 里登记的 host 对得上，newInstanceManager 才会给分支保护 / 状态评审配上
// admin / merge 令牌；仓库清单决定 instanceManagers 展开出几个 Manager。
func giteaFileWithRepos(host string, repos ...string) *instances.File {
	entries := make([]instances.Repo, 0, len(repos))
	for _, name := range repos {
		entries = append(entries, instances.Repo{Name: name})
	}
	return &instances.File{Channels: []instances.Channel{{
		Type:     instances.ChannelGitea,
		Host:     host,
		Reviewer: "ai",
		Merger:   "merge",
		Repos:    entries,
	}}}
}

// isolateCredentials 把凭据库指向一个只属于本测试的空文件。凭据是用户级状态，
// 默认落点跨测试共享；只有显式隔离才能断言「缺凭据」这类分支（否则会读到别的
// 测试写下的令牌，测出来的行为随执行顺序漂移）。
func isolateCredentials(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	return path
}

// TestInstanceManagersExpandsByInstanceAndRepo 断言「实例 × 仓库」的展开契约：
// repoFilter 为空时，仓库清单非空的通道按每个仓库各出一个 Manager，清单为空的
// 通道出一个不限定仓库的 Manager（同步令牌可见的全部仓库）；repoFilter 命中
// 时只保留该仓库。搞错这一条会让 label-sync 之类的动作要么漏掉仓库，要么对着
// 同一个仓库重复执行。
func TestInstanceManagersExpandsByInstanceAndRepo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const host = "https://gitea.example.com"
	writePurposeCredentials(t, []credentials.Credential{
		{Host: host, User: "ai", Purpose: credentials.PurposeReview, Token: "review-token"},
		{Host: "https://other.example.com", User: "ai", Purpose: credentials.PurposeReview, Token: "other-token"},
	})
	file := &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Host: host, Reviewer: "ai", Repos: []instances.Repo{{Name: "acme/video"}, {Name: "acme/audio"}}},
		{Type: instances.ChannelGitea, Host: "https://other.example.com", Reviewer: "ai"},
		// 非 gitea 通道不是仓库机器人身份，绝不能展开成 Manager。
		{Type: instances.ChannelTelegram, BotToken: "t"},
	}}
	var logs bytes.Buffer
	managers, err := instanceManagers(t.Context(), "", file, "", &logs)
	if err != nil {
		t.Fatalf("instanceManagers: %v", err)
	}
	if len(managers) != 3 {
		t.Fatalf("managers = %d, want 3（两个仓库 + 一个不限仓库）", len(managers))
	}
	// 进度回调按实例加前缀：多实例日志混在一处时，操作者靠它分辨是谁在动。
	if !strings.Contains(logs.String(), "["+host+"]") {
		t.Errorf("日志缺少按实例前缀的行：\n%s", logs.String())
	}

	// 单仓库过滤：只应剩一个 Manager。
	filtered, err := instanceManagers(t.Context(), "", file, "acme/audio", io.Discard)
	if err != nil {
		t.Fatalf("instanceManagers(过滤): %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("过滤后 managers = %d, want 1", len(filtered))
	}
}

// TestInstanceManagersRejectsUnknownRepoAndEmptyConfig 断言两条「没有任何可执行
// 目标」的错误信息：过滤词一个都没命中时必须点名仓库（否则操作者以为动作跑了
// 一遍），配置里根本没有 gitea 通道时也要明说。这两种情形都是配置写错，静默
// 成功比报错更危险。
func TestInstanceManagersRejectsUnknownRepoAndEmptyConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	isolateCredentials(t)
	file := giteaFileWithRepos("https://gitea.example.com", "acme/video")
	_, err := instanceManagers(t.Context(), "", file, "acme/nope", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "不在配置文件的 instances[].repos 中") {
		t.Errorf("未命中过滤词时 err = %v", err)
	}
	if _, err := instanceManagers(t.Context(), "", &instances.File{}, "", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "没有可执行的仓库") {
		t.Errorf("空配置 err = %v", err)
	}
}

// TestNewInstanceManagerWiresPurposeTokens 断言按用途取令牌的接线：review 用途
// 令牌是客户端的基线身份，admin 用途令牌只配给分支保护读取，merge 用途令牌
// 只配给状态评审。用错用途会让内容评审以管理员身份提交（official review 的
//
//	authorship 就错了），这是评审状态机的硬约定。
func TestNewInstanceManagerWiresPurposeTokens(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const host = "https://gitea.example.com"
	writePurposeCredentials(t, []credentials.Credential{
		{Host: host, User: "ai", Purpose: credentials.PurposeReview, Token: "review-token"},
		{Host: host, User: "admin", Purpose: credentials.PurposeAdmin, Token: "admin-token"},
		{Host: host, User: "merge", Purpose: credentials.PurposeMerge, Token: "merge-token"},
	})

	var logs bytes.Buffer
	instance := instances.Instance{Host: host, Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"}}
	manager, err := newInstanceManager(t.Context(), "", instance, instances.Repo{Name: "acme/video"}, func(format string, arguments ...any) {
		fmt.Fprintf(&logs, host+" "+format+"\n", arguments...)
	})
	if err != nil {
		t.Fatalf("newInstanceManager: %v", err)
	}
	if manager == nil {
		t.Fatal("manager 为 nil")
	}
	got := logs.String()
	if !strings.Contains(got, "分支保护读取使用 admin 令牌") {
		t.Errorf("缺少 admin 令牌日志：\n%s", got)
	}
	if strings.Contains(got, "没有 admin 用途令牌") || strings.Contains(got, "没有 merge 用途令牌") {
		t.Errorf("三条用途凭据齐全时不该报缺失：\n%s", got)
	}

	// 只有 review 令牌：两个可选用途都要降级并各留一条可行动日志，而不是报错。
	only := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(only, "credentials.json"))
	store := credentials.File{}
	store.SetCredential(credentials.Credential{Host: host, User: "ai", Purpose: credentials.PurposeReview, Token: "review-token"})
	if err := credentials.Save(filepath.Join(only, "credentials.json"), &store); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if _, err := newInstanceManager(t.Context(), "", instance, instances.Repo{}, func(format string, arguments ...any) {
		fmt.Fprintf(&logs, format+"\n", arguments...)
	}); err != nil {
		t.Fatalf("newInstanceManager(仅 review): %v", err)
	}
	got = logs.String()
	if !strings.Contains(got, "将回退严格模式") || !strings.Contains(got, "状态驳回将以基础令牌身份提交") {
		t.Errorf("缺 admin/merge 令牌时降级日志不全：\n%s", got)
	}
}

// TestNewInstanceManagerRejectsBadRepoAndMissingReviewToken 断言两条前置错误：
// 仓库名不是 owner/name 时直接报错（后续所有 API 调用都依赖它），完全没有
// review 用途令牌时也要报错而不是拿空令牌去请求（空令牌只会换来 401，把
// 「凭据没配」误报成「站点拒绝」）。
func TestNewInstanceManagerRejectsBadRepoAndMissingReviewToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const host = "https://gitea.example.com"
	writePurposeCredentials(t, []credentials.Credential{
		{Host: host, User: "ai", Purpose: credentials.PurposeReview, Token: "review-token"},
	})
	instance := instances.Instance{Host: host, Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"}}
	// newInstanceManager 在校验仓库名之前就会调 logf 记降级日志，nil 会直接 panic，
	// 所以这里给一个什么都不做的回调。
	logf := func(string, ...any) {}
	if _, err := newInstanceManager(t.Context(), "", instance, instances.Repo{Name: "not-a-full-name"}, logf); err == nil {
		t.Error("非法仓库名应报错")
	}
	// 换一个没有登记凭据的 host：tokenForPurpose 应报缺失。凭据库必须隔离，
	// 否则可能读到别的测试写下的同 host 令牌，这条断言随执行顺序漂移。
	isolateCredentials(t)
	bare := instances.Instance{Host: "https://bare.example.com", Reviewer: instances.Account{Name: "ai"}}
	if _, err := newInstanceManager(t.Context(), "", bare, instances.Repo{}, logf); err == nil {
		t.Error("没有 review 凭据时应报错")
	}
}

// TestRunManagerActionAggregatesEveryError 断言动作错误的聚合语义：一个仓库
// 失败不影响其余仓库继续执行（一次 label-sync 不该因为某个仓库权限不足而
// 整轮放弃），但所有失败都要出现在返回的错误里，让退出码与报错一致。
func TestRunManagerActionAggregatesEveryError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const host = "https://gitea.example.com"
	writePurposeCredentials(t, []credentials.Credential{
		{Host: host, User: "ai", Purpose: credentials.PurposeReview, Token: "review-token"},
	})
	file := giteaFileWithRepos(host, "acme/video", "acme/audio")
	var visited []string
	failure := errors.New("模拟动作失败")
	err := runManagerAction(t.Context(), io.Discard, commandOptions{}, file, func(_ context.Context, manager *status.Manager) error {
		visited = append(visited, "x")
		return failure
	})
	if len(visited) != 2 {
		t.Fatalf("动作执行 %d 次, want 2（每个仓库都要跑）", len(visited))
	}
	if !errors.Is(err, failure) {
		t.Errorf("err = %v, want 包住动作错误", err)
	}

	// 过滤词没命中时 instanceManagers 的错误必须原样冒泡，动作一次都不跑。
	visited = nil
	err = runManagerAction(t.Context(), io.Discard, commandOptions{Repository: "acme/nope"}, file,
		func(context.Context, *status.Manager) error { visited = append(visited, "y"); return nil })
	if err == nil || len(visited) != 0 {
		t.Errorf("过滤未命中时 err = %v, 执行次数 = %d", err, len(visited))
	}
}
