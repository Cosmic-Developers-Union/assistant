package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleTranscript 是一段真实的 claude 文本记录：user 纯文本、assistant 内容块
// （text + tool_use）、user 的 tool_result、system 的 api_error。抽取与归档测试
// 共用它，保证覆盖到各类消息形态。
const sampleTranscript = `{"type":"user","message":{"role":"user","content":"你好，帮我查队列"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"待评审的队列如下"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"a.jsonl\nb.jsonl"}]}}
{"type":"system","subtype":"api_error","error":{"message":"网关超时"}}`

func osMkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }

// 抽取工具的边角：toolResultText/compactJSON/snippet/DetectSource/intValue/
// conversationLabel/sortStrings 的退化输入。
func TestExtractHelperEdges(t *testing.T) {
	// toolResultText：字符串、内容块数组、空、坏档
	if got := toolResultText(json.RawMessage(`"直接文本"`)); got != "直接文本" {
		t.Errorf("toolResultText(string) = %q", got)
	}
	if got := toolResultText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":""},{"type":"text","text":"b"}]`)); got != "a b" {
		t.Errorf("toolResultText(blocks) = %q", got)
	}
	if got := toolResultText(json.RawMessage(`  `)); got != "" {
		t.Errorf("toolResultText(空) = %q", got)
	}
	if got := toolResultText(json.RawMessage(`123`)); got != "" {
		t.Errorf("toolResultText(不是文本) = %q", got)
	}

	// compactJSON：空、坏档、超长被截断加省略号
	if got := compactJSON(json.RawMessage(`   `)); got != "" {
		t.Errorf("compactJSON(空) = %q", got)
	}
	if got := compactJSON(json.RawMessage(`{`)); got != "" {
		t.Errorf("compactJSON(坏档) = %q", got)
	}
	long := `{"key":"` + strings.Repeat("字", 300) + `"}`
	if got := compactJSON(json.RawMessage(long)); !strings.HasSuffix(got, "…") || len([]rune(got)) != 201 {
		t.Errorf("compactJSON(超长) 长度 = %d，尾巴 = %q", len([]rune(got)), got)
	}

	// snippet：折行压成单空格、超长加省略号
	if got := snippet("a\n\n  b\tc", 100); got != "a b c" {
		t.Errorf("snippet = %q", got)
	}
	if got := snippet(strings.Repeat("字", 10), 4); got != "字字字字…" {
		t.Errorf("snippet(截断) = %q", got)
	}

	// DetectSource：chat 优先、assistant- 前缀是评审、空项目名是空、其它 unknown
	cases := map[string]string{
		"assistant-chat":               "chat",
		"assistant-node-1-acme-rocket": "review",
		"":                             "",
		"  ":                           "",
		"-tmp-somewhere":               "unknown",
	}
	for project, want := range cases {
		if got := DetectSource(project); got != want {
			t.Errorf("DetectSource(%q) = %q, want %q", project, got, want)
		}
	}

	// intValue：float64 / 数字字符串 / 不可解析回落
	if got := intValue(float64(7), 1); got != 7 {
		t.Errorf("intValue(float64) = %d", got)
	}
	if got := intValue(" 9 ", 1); got != 9 {
		t.Errorf("intValue(string) = %d", got)
	}
	if got := intValue("abc", 3); got != 3 {
		t.Errorf("intValue(坏字符串) = %d", got)
	}
	if got := intValue(nil, 5); got != 5 {
		t.Errorf("intValue(nil) = %d", got)
	}

	// conversationLabel / sortStrings
	if got := conversationLabel(""); got != "" {
		t.Errorf("conversationLabel(空) = %q", got)
	}
	if got := conversationLabel("c-1"); got != " 会话=c-1" {
		t.Errorf("conversationLabel = %q", got)
	}
	values := []string{"b", "a", "c", "a"}
	sortStrings(values)
	if strings.Join(values, ",") != "a,a,b,c" {
		t.Errorf("sortStrings = %v", values)
	}
}

// ExtractMessages 对各类行的抽取：元数据行返回空、一行的多内容块、坏档。
func TestExtractMessagesShapes(t *testing.T) {
	if got := ExtractMessages([]byte(`{"type":"summary","summary":"压缩摘要"}`), 0); len(got) != 1 || got[0].Role != "summary" {
		t.Errorf("summary = %+v", got)
	}
	if got := ExtractMessages([]byte(`{"type":"last-prompt","x":1}`), 0); len(got) != 0 {
		t.Errorf("元数据行应返回空：%+v", got)
	}
	if got := ExtractMessages([]byte(`不是 JSON`), 0); len(got) != 0 {
		t.Errorf("坏档应返回空：%+v", got)
	}
	if got := ExtractMessages([]byte(`   `), 0); len(got) != 0 {
		t.Errorf("空行应返回空：%+v", got)
	}
	// 一行可含多条消息（assistant 的 text + tool_use）
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"好的"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	got := ExtractMessages([]byte(line), 3)
	if len(got) != 2 || got[0].Role != "assistant" || got[0].Line != 3 || got[1].Role != "tool" {
		t.Errorf("多内容块抽取 = %+v", got)
	}
}

// sampleLines 把 sampleTranscript 切成行（归档测试用）。
func sampleLines() []string { return strings.Split(sampleTranscript, "\n") }

// sampleClaudeRoot 造一个最小 claude 记录根：projects/<项目>/<会话>.jsonl。
func sampleClaudeRoot(t *testing.T, project, session, transcript string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "claude")
	projectDir := filepath.Join(root, "projects", project)
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, session+".jsonl"), transcript); err != nil {
		t.Fatal(err)
	}
	return root
}
