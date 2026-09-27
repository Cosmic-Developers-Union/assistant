package qq

import "testing"

// 平台投递的文本清理：去首尾空白与残留的 @ 提及前缀（协议层逻辑，随包走）。
func TestCleanQQContent(t *testing.T) {
	cases := map[string]string{
		"<@!BOT123> 查状态":      "查状态",
		"<@BOT123>你好":         "你好",
		"@机器人 帮我看看":           "帮我看看",
		"  纯文本 ":              "纯文本",
		"两段<@A> <@B> mention": "两段<@A> <@B> mention",
	}
	for input, want := range cases {
		if got := cleanQQContent(input); got != want {
			t.Errorf("cleanQQContent(%q) = %q, want %q", input, got, want)
		}
	}
}
