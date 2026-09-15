package conversations

import (
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
