package integration

import (
	"context"
	"testing"
	"time"
)

// SleepCtx 的正常路径：context 未取消时必须真的等满时长再返回（微信 ret=-14
// 的重试节流靠它，提前返回会变成热循环打爆平台限频）。
func TestSleepCtxWaitsOutFullTimer(t *testing.T) {
	start := time.Now()
	SleepCtx(t.Context(), 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("应等满 50ms，实际只等了 %v", elapsed)
	}
}

// SleepCtx 的取消路径：context 取消后必须立刻返回而不是继续睡——daemon 退出时
// 通道协程要能马上收尾，否则最长会拖住一小时的退避睡眠（微信 ret=-14 分支）。
func TestSleepCtxReturnsImmediatelyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	SleepCtx(ctx, time.Hour)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("已取消的 context 应立即返回，实际等了 %v", elapsed)
	}
}

// 控制命令解析：斜杠与直白说法都认，普通聊天不受影响。
func TestParseCommand(t *testing.T) {
	for text, want := range map[string]Command{
		"/new":     {Name: "reset"},
		" /reset ": {Name: "reset"},
		"重新开始":     {Name: "reset"},
		"新会话":      {Name: "reset"},
		"/clear":   {Name: "reset"},
		"/help":    {Name: "help"},
		"帮助":       {Name: "help"},
	} {
		if got := ParseCommand(text); got != want {
			t.Errorf("ParseCommand(%q) = %+v, want %+v", text, got, want)
		}
	}
	for _, text := range []string{"继续", "new", "重新开始吧，但先回答我", "/news", "/agents", "/agentx ops", ""} {
		if got := ParseCommand(text); got.Name != "" {
			t.Errorf("ParseCommand(%q) 不该识别为命令：+%v", text, got)
		}
	}
}

// 切块逻辑：按 rune 切、尽量在换行处切、短文本不切。
func TestSplitText(t *testing.T) {
	chunks := SplitText("第一行\n第二行内容很长", 5)
	if len(chunks) < 2 || chunks[0] != "第一行" {
		t.Errorf("SplitText = %q", chunks)
	}
	if chunks := SplitText("短", 10); len(chunks) != 1 || chunks[0] != "短" {
		t.Errorf("短文本不该切：%q", chunks)
	}
	if chunks := SplitText("无换行的长文本内容", 5); len(chunks) != 2 {
		t.Errorf("无换行也要能切：%q", chunks)
	}
	if chunks := SplitText("任意", 0); len(chunks) != 1 {
		t.Errorf("上限 <=0 表示不切：%q", chunks)
	}
}

// 三平台逐字相同的准入算法：通配 → 白名单 → 拒。各平台只差拒绝措辞。
func TestAllowedByWhitelist(t *testing.T) {
	if allow, _ := AllowedByWhitelist([]string{"*"}, "anyone", "no-list"); !allow {
		t.Error(`"*" 应放开所有人`)
	}
	if allow, _ := AllowedByWhitelist([]string{"a", "b"}, "b", "no-list"); !allow {
		t.Error("白名单内用户应放行")
	}
	if allow, reason := AllowedByWhitelist([]string{"a"}, "c", "no-list"); allow || reason != "白名单外用户" {
		t.Errorf("白名单外应拒绝并说明：allow=%v reason=%q", allow, reason)
	}
	// 空白名单：用调用方给的措辞（各平台不同）
	if allow, reason := AllowedByWhitelist(nil, "a", "未配置 qq.admin_users"); allow || reason != "未配置 qq.admin_users" {
		t.Errorf("空白名单应用传入措辞：allow=%v reason=%q", allow, reason)
	}
	// 空用户标识优先于白名单判定
	if allow, reason := AllowedByWhitelist([]string{"*"}, "  ", "no-list"); allow || reason != "用户标识为空" {
		t.Errorf("空用户标识应拒绝：allow=%v reason=%q", allow, reason)
	}
}
