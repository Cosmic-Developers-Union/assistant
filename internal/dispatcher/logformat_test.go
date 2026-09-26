package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
)

// TestDescribeToolInput 覆盖工具输入摘要的格式化规则：键优先级、空值跳过、
// 多值拼接、换行折平、超长截断（按 rune 而不是字节）。
func TestDescribeToolInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "空输入", raw: "", want: ""},
		{name: "非法 JSON", raw: "{", want: ""},
		{name: "空对象", raw: "{}", want: ""},
		{name: "单键 command", raw: `{"command":"git status"}`, want: "git status"},
		{name: "数字键转字符串", raw: `{"number":58}`, want: "58"},
		{name: "空白值跳过", raw: `{"file_path":"  "}`, want: ""},
		{name: "顶层非对象", raw: `"裸字符串"`, want: ""},
		{
			name: "多键按优先级拼接",
			raw:  `{"path":"/tmp/a","pattern":"triage","extra":"忽略"}`,
			want: "/tmp/a · triage",
		},
		{
			name: "换行折平为空格",
			raw:  `{"command":"line1\nline2"}`,
			want: "line1 line2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claude.DescribeToolInput("tool", json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("claude.DescribeToolInput(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// 截断按 rune 计数：多字节字符不得被从中间截断（按字节切会产生乱码）。
func TestDescribeToolInputTruncatesByRunes(t *testing.T) {
	exactly := strings.Repeat("中", 120)
	if got := claude.DescribeToolInput("tool", json.RawMessage(`{"command":"`+exactly+`"}`)); got != exactly {
		t.Errorf("恰好 120 字不得截断，got %q", got)
	}
	over := strings.Repeat("中", 121)
	got := claude.DescribeToolInput("tool", json.RawMessage(`{"command":"`+over+`"}`))
	if got != strings.Repeat("中", 120)+"…" {
		t.Errorf("超长需按 rune 截断并加省略号，got %q", got)
	}
	if strings.Count(got, "中") != 120 {
		t.Errorf("截断处多字节字符被切碎：%q", got)
	}
}

// body/prompt 的长度标注即工具输入摘要的语义：长正文只报规模不刷屏。
func TestDescribeToolInputLabelsBodyLength(t *testing.T) {
	got := claude.DescribeToolInput("tool", json.RawMessage(`{"body":"短正文"}`))
	if got != "body(3 字)" {
		t.Errorf("describeToolInput = %q, want body(3 字)", got)
	}
	// 恰好 120 字不截断
	exactly := strings.Repeat("文", 120)
	if got = claude.DescribeToolInput("tool", json.RawMessage(`{"body":"`+exactly+`"}`)); got != "body(120 字)" {
		t.Errorf("describeToolInput = %q, want body(120 字)", got)
	}
	// 超过 120 字：标注的是截断后的长度（含省略号），语义是「这是长正文」
	over := strings.Repeat("文", 200)
	if got = claude.DescribeToolInput("tool", json.RawMessage(`{"body":"`+over+`"}`)); got != "body(121 字)" {
		t.Errorf("describeToolInput = %q, want body(121 字)", got)
	}
}

// 摘要最多并列 3 个键，避免一行日志被工具参数淹没。
func TestDescribeToolInputCapsAtThreeParts(t *testing.T) {
	raw := json.RawMessage(`{"command":"a","file_path":"b","path":"c","pattern":"d"}`)
	if got, want := claude.DescribeToolInput("tool", raw), "a · b · c"; got != want {
		t.Errorf("claude.DescribeToolInput() = %q, want %q", got, want)
	}
}

// TestDescribeToolResult 覆盖工具结果摘要：成功按内容形态给出字数，失败以 ✗ 前缀
// 并携带错误正文。
func TestDescribeToolResult(t *testing.T) {
	isError := true
	cases := []struct {
		name  string
		block claude.ContentBlock
		want  string
	}{
		{
			name:  "无内容完成",
			block: claude.ContentBlock{},
			want:  "（完成）",
		},
		{
			name:  "纯文本结果按 rune 记数",
			block: claude.ContentBlock{Text: "已完成评审"},
			want:  "（返回 5 字）",
		},
		{
			name:  "结构化内容按字节记数",
			block: claude.ContentBlock{Content: json.RawMessage(`{"ok":true}`)},
			want:  "（返回 11 字）",
		},
		{
			// Content 非空时优先：结构化的 tool_result 正文在 Content 里
			name:  "内容优先于文本",
			block: claude.ContentBlock{Text: "abc", Content: json.RawMessage(`{"ok":true}`)},
			want:  "（返回 11 字）",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := tc.block
			if got := claude.DescribeToolResult(&block); got != tc.want {
				t.Errorf("claude.DescribeToolResult() = %q, want %q", got, tc.want)
			}
		})
	}

	errBlock := claude.ContentBlock{IsError: &isError, Text: "  permission denied\ncheck token  "}
	if got := claude.DescribeToolResult(&errBlock); got != "✗ permission denied check token" {
		t.Errorf("错误结果 = %q, want ✗ 前缀 + 折平后的正文", got)
	}

	emptyErr := claude.ContentBlock{IsError: new(true)}
	if got := claude.DescribeToolResult(&emptyErr); got != "✗ " {
		t.Errorf("无正文的错误结果 = %q, want 「✗ 」", got)
	}

	rawErr := claude.ContentBlock{IsError: new(true), Content: json.RawMessage(`{"error":"boom"}`)}
	if got := claude.DescribeToolResult(&rawErr); got != `✗ {"error":"boom"}` {
		t.Errorf("错误结果缺 Text 时回退 Content = %q", got)
	}

	// 错误正文超长按 rune 截断到 160 字 + 省略号
	long := claude.ContentBlock{IsError: new(true), Text: strings.Repeat("错", 200)}
	if got := claude.DescribeToolResult(&long); got != "✗ "+strings.Repeat("错", 160)+"…" {
		t.Errorf("超长错误正文 = %q, want 截断到 160 字", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{name: "短于上限原样返回", text: "abc", limit: 5, want: "abc"},
		{name: "恰好等于上限不截断", text: "abcde", limit: 5, want: "abcde"},
		{name: "超一位即截断", text: "abcdef", limit: 5, want: "abcde…"},
		{name: "空串", text: "", limit: 0, want: ""},
		{name: "零上限", text: "中文", limit: 0, want: "…"},
		{name: "多字节按 rune 截断", text: "中文测试", limit: 2, want: "中文…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.text, tc.limit); got != tc.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.text, tc.limit, got, tc.want)
			}
		})
	}
}

func TestDebugProgressPrefixAndNilGuard(t *testing.T) {
	var lines []string
	debugProgress(func(line string) { lines = append(lines, line) }, "会话命令行")
	if len(lines) != 1 || lines[0] != "[debug] 会话命令行" {
		t.Errorf("lines = %v, want [debug] 前缀", lines)
	}

	debugProgress(func(string) { t.Error("空行不得上报") }, "")
	debugProgress(nil, "无回调不得 panic")
}

func TestDescribeSessionMCP(t *testing.T) {
	t.Run("空路径", func(t *testing.T) {
		for _, path := range []string{"", "   "} {
			got := describeSessionMCP(path)
			if len(got) != 1 || got[0] != "（无）" {
				t.Errorf("describeSessionMCP(%q) = %v, want （无）", path, got)
			}
		}
	})

	t.Run("读取失败", func(t *testing.T) {
		got := describeSessionMCP(filepath.Join(t.TempDir(), "missing.json"))
		if len(got) != 1 || !strings.HasPrefix(got[0], "（读取失败：") {
			t.Errorf("describeSessionMCP() = %v, want 读取失败", got)
		}
	})

	t.Run("解析失败", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "broken.json")
		if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := describeSessionMCP(path)
		if len(got) != 1 || !strings.HasPrefix(got[0], "（解析失败：") {
			t.Errorf("describeSessionMCP() = %v, want 解析失败", got)
		}
	})

	t.Run("正常解析", func(t *testing.T) {
		document := `{"mcpServers":{"gitea":{"command":"assistant","args":["mcp","gitea"]}}}`
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
			t.Fatal(err)
		}
		got := describeSessionMCP(path)
		if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "gitea") {
			t.Errorf("describeSessionMCP() = %v, want 含 gitea 服务器", got)
		}
	})
}
