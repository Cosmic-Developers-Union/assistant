package conversations

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Load 的退化契约：文件不存在是空表（不是错误）、坏档必须**报错**而不是被当成
// 空表（把损坏的映射当空会静默丢会话）、版本 0 补成 CurrentVersion。
func TestLoadDegenerateFile(t *testing.T) {
	// 不存在的文件：空表 + 当前版本，不报错
	file, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || file == nil {
		t.Fatalf("缺失文件应返回空表：%+v err=%v", file, err)
	}
	if file.Version != CurrentVersion || len(file.Conversations) != 0 {
		t.Errorf("空表 = %+v", file)
	}

	// 坏档：报错并带上路径，绝不静默当空
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if parsed, err := Load(broken); err == nil || parsed != nil {
		t.Fatalf("坏档应报错且不返回表：%+v err=%v", parsed, err)
	} else if !strings.Contains(err.Error(), broken) {
		t.Errorf("错误应带上路径：%v", err)
	}

	// 版本 0（缺 version 字段）：补成 CurrentVersion
	bare := filepath.Join(t.TempDir(), "versionless.json")
	if err := os.WriteFile(bare, []byte(`{"conversations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if parsed, err := Load(bare); err != nil || parsed.Version != CurrentVersion {
		t.Errorf("版本 0 应补成 %d：%+v err=%v", CurrentVersion, parsed, err)
	}

	// 路径是目录：读失败不该被当成「不存在」而放过
	if parsed, err := Load(t.TempDir()); err == nil || parsed != nil {
		t.Errorf("路径是目录时应报错：%+v err=%v", parsed, err)
	}
}

// Save 的错误路径：目标目录不可创建（父路径是普通文件）返回错误、成功时原子替换
// 不留 .tmp、权限 0600、版本被钉到当前。
func TestSaveErrorPaths(t *testing.T) {
	dir := t.TempDir()

	file := &File{}
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(blocked, "conversations.json"), file); err == nil {
		t.Error("父路径是普通文件时应报错")
	}

	// 成功路径：缩进两空格、换行结尾、0600、无 .tmp 残留、版本被钉到当前
	path := filepath.Join(dir, "nested", "conversations.json")
	if _, created := file.Ensure("weixin", "u1", "标题"); !created {
		t.Fatal("应新建会话")
	}
	file.Version = 0
	if err := Save(path, file); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("权限 = %v，应为 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("应原子替换，不该留下 .tmp：%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") || !strings.Contains(string(data), "\n  \"version\": 2") {
		t.Errorf("落盘形态不对：\n%s", data)
	}
}

// ByID / AgentOf / Find 的未命中：都返回零值且第二个返回 false，不 panic。
func TestLookupMisses(t *testing.T) {
	file := &File{}
	if conversation, ok := file.ByID("c-nope"); ok || conversation.ID != "" {
		t.Errorf("ByID 未命中应返回零值：%+v ok=%v", conversation, ok)
	}
	if got := file.AgentOf("c-nope"); got != "" {
		t.Errorf("AgentOf 未命中应为空：%q", got)
	}
	if conversation, ok := file.Find("weixin", "nobody"); ok || conversation.ID != "" {
		t.Errorf("Find 未命中应返回零值：%+v ok=%v", conversation, ok)
	}
	// 零值 File 上的查询同样安全
	var zero File
	if _, ok := zero.ByID("x"); ok {
		t.Error("零值 File 的 ByID 不该命中")
	}
	if got := zero.AgentOf("x"); got != "" {
		t.Errorf("零值 File 的 AgentOf = %q", got)
	}
}

// Ensure / Bind / Attach / SetAgent 的空参数与未知会话边界：空通道/用户视作失败
// 而不是建出一个无绑定会话。
func TestMutationBlankArgs(t *testing.T) {
	file := &File{}
	if conversation, created := file.Ensure("  ", "u1", ""); created || conversation.ID != "" {
		t.Errorf("空通道不该建会话：%+v created=%v", conversation, created)
	}
	if conversation, created := file.Ensure("weixin", "\t", ""); created || conversation.ID != "" {
		t.Errorf("空用户不该建会话：%+v created=%v", conversation, created)
	}
	if len(file.Conversations) != 0 {
		t.Errorf("失败的 Ensure 不该留下记录：%+v", file.Conversations)
	}

	if err := file.Bind("c-any", "  ", "u1"); err == nil {
		t.Error("空通道的 Bind 应报错")
	}
	if err := file.Bind("c-any", "weixin", ""); err == nil {
		t.Error("空用户的 Bind 应报错")
	}

	created, _ := file.Ensure("weixin", "u1", "  标题  ")
	if created.Title != "标题" {
		t.Errorf("标题应去空白：%q", created.Title)
	}
	if err := file.Attach(created.ID, "   "); err == nil {
		t.Error("空会话 id 的 Attach 应报错")
	}
	if err := file.Attach("c-nope", "s-1"); err == nil {
		t.Error("未知会话的 Attach 应报错")
	}
	if err := file.SetAgent("c-nope", "ops"); err == nil {
		t.Error("未知会话的 SetAgent 应报错")
	}

	// 重复绑定同一通道/用户到同一会话：幂等返回 nil，不追加第二条
	if err := file.Bind(created.ID, "telegram", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := file.Bind(created.ID, "telegram", "t1"); err != nil {
		t.Fatalf("重复 Bind 应幂等：%v", err)
	}
	stored, _ := file.ByID(created.ID)
	if len(stored.Bindings) != 2 {
		t.Errorf("重复 Bind 不该追加：%+v", stored.Bindings)
	}
	// 同一通道/用户绑定到**另一个**会话：冲突报错
	other, _ := file.Ensure("qq", "q1", "")
	if err := file.Bind(other.ID, "telegram", "t1"); err == nil {
		t.Error("同一绑定指向另一会话应报错")
	}
}

// 会话 id 撞车时的加后缀分支：不同通道/用户恰好派生同一个 id 时退化为加后缀，
// 两条记录都保留且可分别取回。
func TestEnsureIDCollisionSuffix(t *testing.T) {
	file := &File{}
	first, _ := file.Ensure("weixin", "u1", "")
	// 手工塞一条与派生结果同 id 的记录，触发下一次 Ensure 的后缀分支
	file.Conversations[0].ID = deriveID("telegram", "t1")
	// 现在文件里已有 telegram/t1 的 id，但 Find(telegram,t1) 仍未命中（绑定还是 weixin/u1）
	if found, ok := file.Find("telegram", "t1"); ok {
		t.Fatalf("前置条件不对：%+v", found)
	}
	second, created := file.Ensure("telegram", "t1", "")
	if !created {
		t.Fatal("应新建会话")
	}
	if second.ID == first.ID {
		t.Fatalf("撞车应加后缀：%q", second.ID)
	}
	if !strings.HasPrefix(second.ID, deriveID("telegram", "t1")+"-") {
		t.Errorf("后缀形态不对：%q", second.ID)
	}
	if len(file.Conversations) != 2 {
		t.Errorf("两条记录都应保留：%+v", file.Conversations)
	}
	if found, ok := file.Find("telegram", "t1"); !ok || found.ID != second.ID {
		t.Errorf("反查应命中带后缀的那条：%+v ok=%v", found, ok)
	}
}

// BindingLabels 的排序与空绑定：空绑定的会话返回空切片（长度 0，非 nil）。
func TestBindingLabelsEdges(t *testing.T) {
	var empty Conversation
	if labels := empty.BindingLabels(); len(labels) != 0 {
		t.Errorf("无绑定应返回空切片：%v", labels)
	}
	conversation := Conversation{Bindings: []Binding{
		{Transport: "weixin", User: "u1"},
		{Transport: "telegram", User: "t1"},
		{Transport: "qq", User: "q1"},
	}}
	labels := conversation.BindingLabels()
	if strings.Join(labels, ",") != "qq:q1,telegram:t1,weixin:u1" {
		t.Errorf("应按标签排序：%v", labels)
	}
}

// 读错误（不是「不存在」）必须原样上报：拿一个无读权限的文件确认错误链里能看到
// 文件系统错误，而不是被折成空表。
func TestLoadReadPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视文件权限，跳过")
	}
	path := filepath.Join(t.TempDir(), "locked.json")
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o000); err != nil {
		t.Fatal(err)
	}
	parsed, err := Load(path)
	if err == nil {
		t.Fatalf("无读权限应报错：%+v", parsed)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("错误应可识别为文件系统错误：%v", err)
	}
}
