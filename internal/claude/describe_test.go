package claude

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonRaw 把任意值编成 json.RawMessage（构造入参用）。
func jsonRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// describeToolInput 提炼最有判断价值的字段：路径/编号/命令，按优先级取前三个。
func TestDescribeToolInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		input any
		want  []string
		empty bool
	}{
		{
			name:  "命令优先",
			input: map[string]any{"command": "go test ./...", "description": "跑测试"},
			want:  []string{"go test ./...", "跑测试"},
		},
		{
			name:  "文件路径",
			input: map[string]any{"file_path": "/work/main.go"},
			want:  []string{"/work/main.go"},
		},
		{
			name:  "Gitea 工具的组合键",
			input: map[string]any{"owner": "acme", "repo": "video", "number": 42},
			want:  []string{"acme", "video", "42"},
		},
		{
			name:  "最多三键",
			input: map[string]any{"command": "a", "file_path": "b", "path": "c", "pattern": "d"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "长文本只报字数",
			input: map[string]any{"body": strings.Repeat("字", 300)},
			// 先把值截到 120 字再加省略号，所以计数是 121（含那个 …）——这是既有
			// 语义：字数只用来判断规模，不求精确。
			want: []string{"body(121 字)"},
		},
		{name: "空对象无可摘要", input: map[string]any{}, empty: true},
		{name: "无关键键", input: map[string]any{"unknown_field": "x"}, empty: true},
		{name: "空值被跳过", input: map[string]any{"command": "  ", "path": "/a"}, want: []string{"/a"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := DescribeToolInput("T", jsonRaw(t, test.input))
			if test.empty {
				if got != "" {
					t.Errorf("应为空摘要：%q", got)
				}
				return
			}
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("摘要缺少 %q：%q", want, got)
				}
			}
		})
	}
}

// 空入参与非法 JSON 都不产出摘要（不能 panic，也不能产出半个破折号尾巴）。
func TestDescribeToolInputDegenerate(t *testing.T) {
	if got := DescribeToolInput("T", nil); got != "" {
		t.Errorf("空入参 = %q", got)
	}
	if got := DescribeToolInput("T", json.RawMessage(`{not json`)); got != "" {
		t.Errorf("非法 JSON = %q", got)
	}
}

// 多行值折成单行（日志一条一行）。
func TestDescribeToolInputFoldsNewlines(t *testing.T) {
	got := DescribeToolInput("Bash", jsonRaw(t, map[string]any{"command": "go build \\\n  ./..."}))
	if strings.Contains(got, "\n") {
		t.Errorf("摘要不应含换行：%q", got)
	}
}

// describeToolResult 折叠回执：错误要显出来，长输出只报规模。
func TestDescribeToolResult(t *testing.T) {
	errorFlag := true
	successFlag := false

	t.Run("成功只报规模", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result", Content: jsonRaw(t, "读取成功")}
		if got := DescribeToolResult(block); !strings.HasPrefix(got, "（返回 ") {
			t.Errorf("成功回执 = %q", got)
		}
	})

	t.Run("错误标 ✗ 并带原因", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result", IsError: &errorFlag, Text: "拒绝访问"}
		got := DescribeToolResult(block)
		if !strings.HasPrefix(got, "✗ ") || !strings.Contains(got, "拒绝访问") {
			t.Errorf("错误回执 = %q", got)
		}
	})

	t.Run("错误原因在 content 里也能取到", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result", IsError: &errorFlag, Content: jsonRaw(t, "从 content 取")}
		if got := DescribeToolResult(block); !strings.Contains(got, "从 content 取") {
			t.Errorf("错误回执 = %q", got)
		}
	})

	t.Run("错误无正文给空原因不炸", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result", IsError: &errorFlag}
		if got := DescribeToolResult(block); !strings.HasPrefix(got, "✗ ") {
			t.Errorf("错误回执 = %q", got)
		}
	})

	t.Run("超长错误截断", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result", IsError: &errorFlag, Text: strings.Repeat("错", 500)}
		if got := DescribeToolResult(block); len([]rune(got)) > toolResultDetailLimit+5 {
			t.Errorf("应截断：%d 字", len([]rune(got)))
		}
	})

	t.Run("成功但用 Text 字段", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result", Text: "文本结果", IsError: &successFlag}
		if got := DescribeToolResult(block); !strings.HasPrefix(got, "（返回 ") {
			t.Errorf("回执 = %q", got)
		}
	})

	t.Run("既无内容也无文本", func(t *testing.T) {
		block := &ContentBlock{Type: "tool_result"}
		if got := DescribeToolResult(block); got != "（完成）" {
			t.Errorf("回执 = %q, want （完成）", got)
		}
	})
}

// ToolUseLines 展开入参为缩进 JSON 多行，并封顶（展开形态用）。
func TestToolUseLines(t *testing.T) {
	t.Run("对象逐行展开", func(t *testing.T) {
		got := ToolUseLines("Read", jsonRaw(t, map[string]any{"file_path": "/a.go"}))
		lines := strings.Split(got, "\n")
		if lines[0] != "🔧 Read" {
			t.Errorf("首行 = %q", lines[0])
		}
		if len(lines) < 2 || !strings.HasPrefix(lines[1], "  ") {
			t.Errorf("入参应缩进展开：%v", lines)
		}
	})

	t.Run("空入参只报工具名", func(t *testing.T) {
		if got := ToolUseLines("Task", nil); got != "🔧 Task" {
			t.Errorf("= %q", got)
		}
	})

	t.Run("非法 JSON 折成单行", func(t *testing.T) {
		got := ToolUseLines("T", json.RawMessage(`{not json`))
		if !strings.HasPrefix(got, "🔧 T\n  ") {
			t.Errorf("= %q", got)
		}
		if strings.Count(got, "\n") != 1 {
			t.Errorf("非法 JSON 应折成一行：%q", got)
		}
	})
}

// toolResultText 取文本：字符串、内容块数组、兜底原文三种形态。
func TestToolResultText(t *testing.T) {
	if got := toolResultText(jsonRaw(t, "纯字符串")); got != "纯字符串" {
		t.Errorf("字符串形态 = %q", got)
	}
	blocks := []map[string]any{
		{"type": "text", "text": "第一段"},
		{"type": "text", "text": "第二段"},
		{"type": "image"},
	}
	if got := toolResultText(jsonRaw(t, blocks)); got != "第一段 第二段" {
		t.Errorf("内容块形态 = %q", got)
	}
	// 兜底：既不是字符串也不是块数组时返回原文
	if got := toolResultText(json.RawMessage(`123`)); got != "123" {
		t.Errorf("兜底形态 = %q", got)
	}
	if got := toolResultText(nil); got != "" {
		t.Errorf("空入参 = %q", got)
	}
}

// truncateRunes 按 rune 截断（不劈开多字节字符），limit<=0 时原样返回。
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("你好世界", 2); got != "你好…" {
		t.Errorf("= %q", got)
	}
	if got := truncateRunes("短", 10); got != "短" {
		t.Errorf("未超限应原样：%q", got)
	}
	if got := truncateRunes("abc", 0); got != "abc" {
		t.Errorf("limit=0 应原样：%q", got)
	}
}

// sortedStrings 就地排序（BrokenMCPServers 的输出顺序靠它稳定）。
func TestSortedStrings(t *testing.T) {
	values := []string{"c", "a", "b"}
	sortedStrings(values)
	if strings.Join(values, ",") != "a,b,c" {
		t.Errorf("= %v", values)
	}
}

// envOrFallback 取环境变量，缺省时回退。
func TestEnvOrFallback(t *testing.T) {
	t.Setenv("ASSISTANT_CLAUDE_TEST", "set")
	if got := envOrFallback("ASSISTANT_CLAUDE_TEST", "fallback"); got != "set" {
		t.Errorf("= %q, want set", got)
	}
	// 未设置时回退
	if got := envOrFallback("ASSISTANT_CLAUDE_ABSENT", "fallback"); got != "fallback" {
		t.Errorf("= %q, want fallback", got)
	}
	// 空白值也算未设置
	t.Setenv("ASSISTANT_CLAUDE_BLANK", "   ")
	if got := envOrFallback("ASSISTANT_CLAUDE_BLANK", "fallback"); got != "fallback" {
		t.Errorf("空白值应回退：%q", got)
	}
}

// textLines 折行并封顶：达上限时追加一行截断标记（不是静默丢弃剩余内容）。
func TestTextLines(t *testing.T) {
	lines := textLines("第一行\n第二行\n第三行", 2, 200)
	if len(lines) != 3 {
		t.Fatalf("2 行上限 + 截断标记 = 3 行：%v", lines)
	}
	if lines[2] != "…（已截断）" {
		t.Errorf("末行应为截断标记：%q", lines[2])
	}

	// 未达上限时原样给出行，无标记
	if got := textLines("只有一行", 5, 200); len(got) != 1 || got[0] != "只有一行" {
		t.Errorf("未超限 = %v", got)
	}

	// 空行被跳过（不产出空展示行）
	if got := textLines("a\n\n   \nb", 10, 200); len(got) != 2 {
		t.Errorf("空行应跳过：%v", got)
	}

	// 宽度截断
	if got := textLines(strings.Repeat("x", 300), 5, 50); len([]rune(got[0])) != 51 {
		t.Errorf("宽度应截断到 50 + 省略号：%d 字", len([]rune(got[0])))
	}
}

// describeInit 在字段缺失时不产出半行（不出现「model = 」这种空值行）。
func TestDescribeInitSkipsEmptyFields(t *testing.T) {
	model := "m"
	event := Event{Model: &model}
	got := describeInit(event)
	if !strings.Contains(got, "model = m") {
		t.Errorf("应含 model：%s", got)
	}
	if strings.Contains(got, "认证 = ") || strings.Contains(got, "cwd = ") {
		t.Errorf("缺失字段不应产出空值行：\n%s", got)
	}
}

// describeInit 报出技能与子代理数量（排查「能力没装上」时的线索）。
func TestDescribeInitCounts(t *testing.T) {
	event := Event{
		Skills: []any{map[string]any{}, map[string]any{}},
		Agents: []any{map[string]any{}},
		MCPServers: []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}{{Name: "gitea", Status: "connected"}},
	}
	got := describeInit(event)
	for _, want := range []string{"技能 = 2 个", "子代理 = 1 个", "MCP = gitea=connected"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q：\n%s", want, got)
		}
	}
}
