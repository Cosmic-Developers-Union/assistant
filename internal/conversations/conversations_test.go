package conversations

import (
	"os"
	"path/filepath"
	"testing"
)

// 映射层：通道绑定 → 会话实体（确定性派生、幂等）；多通道可并入同一会话；claude
// 会话（/new 后是新 id）挂在会话实体下。
func TestConversationMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	first, created := file.Ensure("weixin", "o9cq80yYcUkby1i0GsUNJ1u00RSM@im.wechat", "微信会话")
	if !created || first.ID == "" || len(first.Bindings) != 1 {
		t.Fatalf("Ensure = %+v created=%v", first, created)
	}
	again, created := file.Ensure("weixin", "o9cq80yYcUkby1i0GsUNJ1u00RSM@im.wechat", "")
	if created || again.ID != first.ID {
		t.Fatalf("重复 Ensure 应返回同一会话：%+v created=%v", again, created)
	}
	if found, ok := file.Find("weixin", "o9cq80yYcUkby1i0GsUNJ1u00RSM@im.wechat"); !ok || found.ID != first.ID {
		t.Fatalf("Find = %+v ok=%v", found, ok)
	}

	// 另一个通道的同一人并入同一会话（刻意允许：会话实体不由通道决定）
	if err := file.Bind(first.ID, "telegram", "12345"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if found, ok := file.Find("telegram", "12345"); !ok || found.ID != first.ID {
		t.Fatalf("绑定后反查失败：%+v ok=%v", found, ok)
	}
	if labels := file.Conversations[0].BindingLabels(); len(labels) != 2 {
		t.Errorf("BindingLabels = %v", labels)
	}
	if err := file.Bind("c-nope", "slack", "u1"); err == nil {
		t.Error("绑定到不存在的会话应报错")
	}

	// claude 会话挂载：去重、最新在后
	for _, sessionID := range []string{"s-1", "s-2", "s-1"} {
		if err := file.Attach(first.ID, sessionID); err != nil {
			t.Fatalf("Attach(%s): %v", sessionID, err)
		}
	}
	stored, _ := file.ByID(first.ID)
	if len(stored.Sessions) != 2 || stored.Sessions[0] != "s-1" || stored.Sessions[1] != "s-2" {
		t.Errorf("Sessions = %v", stored.Sessions)
	}

	// 落盘往返
	if err := Save(path, file); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if found, ok := reloaded.Find("weixin", "o9cq80yYcUkby1i0GsUNJ1u00RSM@im.wechat"); !ok || found.ID != first.ID {
		t.Fatalf("重新加载后映射丢失：%+v ok=%v", found, ok)
	}
	if _, ok := reloaded.ByID(first.ID); !ok {
		t.Error("重新加载后会话丢失")
	}
}

// /agent 的会话级选择：设置、覆盖、清除、按 id 反查；v2 字段在 v1 旧文件上向后兼容。
func TestSetAgentAndAgentOf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := file.Ensure("weixin", "user-1", "")
	if err := file.SetAgent(first.ID, "ops"); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}
	if got := file.AgentOf(first.ID); got != "ops" {
		t.Errorf("AgentOf = %q, want ops", got)
	}
	if err := file.SetAgent(first.ID, ""); err != nil {
		t.Fatalf("SetAgent 清除: %v", err)
	}
	if got := file.AgentOf(first.ID); got != "" {
		t.Errorf("清除后 AgentOf = %q", got)
	}
	if err := file.SetAgent("c-nope", "ops"); err == nil {
		t.Error("对不存在的会话 SetAgent 应报错")
	}

	// 落盘往返（v2）
	if err := file.SetAgent(first.ID, "coder"); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, file); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.AgentOf(first.ID); got != "coder" {
		t.Errorf("重新加载后 AgentOf = %q, want coder", got)
	}

	// v1 旧文件（无 agent 字段）读入不报错
	v1 := `{"version":1,"conversations":[{"id":"c-1a2b3c4d","created_at":"2026-01-01T00:00:00Z"}]}` + "\n"
	oldPath := filepath.Join(t.TempDir(), "conversations.json")
	if err := os.WriteFile(oldPath, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(oldPath)
	if err != nil {
		t.Fatalf("v1 文件应可读：%v", err)
	}
	if got := legacy.AgentOf("c-1a2b3c4d"); got != "" {
		t.Errorf("v1 会话的 AgentOf = %q", got)
	}
	if err := legacy.SetAgent("c-1a2b3c4d", "ops"); err != nil {
		t.Fatalf("v1 会话 SetAgent: %v", err)
	}
}
