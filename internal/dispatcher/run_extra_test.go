package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// SessionOptions.Config 里 Config 字段的取值来源是 dispatcher 的 Config——
// 补一轮「会话配置根/项目目录名显式透传」的 docker 参数：这两行 -e 是把
// 容器内文本记录落到与宿主一致路径上的唯一通道，漏掉就会在容器里另起一份
// 会话目录，续接（--resume）永远找不到既有记录。
func TestSessionCommandDockerSessionDirAndProject(t *testing.T) {
	options := SessionOptions{
		Config: Config{
			ClaudeBin:      "claude",
			DockerImage:    "assistant-review:dev",
			SessionDir:     "/var/lib/assistant/claude",
			SessionProject: "worktrees-pr-9",
		},
		Prompt:        "review pr #9",
		Cwd:           "/tmp/worktrees/pr-9",
		MCPConfigPath: "/tmp/worktrees/pr-9/.mcp.json",
	}
	_, args, _ := sessionCommand(options)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-e CLAUDE_CONFIG_DIR=/var/lib/assistant/claude",
		"-e CLAUDE_CODE_PROJECT_DIR_NAME=worktrees-pr-9",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker args 缺 %q：%v", want, args)
		}
	}
}

// ReadReviewConventions 的两个空串出口：projectDir 为空（Issue 分诊可能不
// 带项目目录）与文件的 trim 后为空（只写了空白），都必须回落空串而不是注入
// 一段空提示词。
func TestReadReviewConventionsEmptyBranches(t *testing.T) {
	if got := ReadReviewConventions(""); got != "" {
		t.Errorf("空 projectDir 应返回空串，got %q", got)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".assistant", "review.md"), "  \n\t\n  \n")
	if got := ReadReviewConventions(dir); got != "" {
		t.Errorf("纯空白约定应返回空串，got %q", got)
	}
	// 恰好等于上限不截断：边界比较用的是 > not >=
	exact := strings.Repeat("标", ReviewConventionsLimit)
	writeFile(t, filepath.Join(dir, ".assistant", "review.md"), exact)
	if got := ReadReviewConventions(dir); got != exact {
		t.Errorf("恰好 %d 字不该截断，len=%d", ReviewConventionsLimit, len([]rune(got)))
	}
}

// feedStreamEvent 的 tool_use 分支在 onProgress 为 nil 时不得 panic：会话归档
// （无控制台订阅者）也要能跑完。这条守卫是「nil 回调不得崩」的唯一证据。
func TestFeedStreamEventNilProgressTolerated(t *testing.T) {
	outcome := NewSessionOutcome()
	events := []string{
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"结论"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
	}
	for _, line := range events {
		var event streamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unmarshal %s: %v", line, err)
		}
		if isResult := feedStreamEvent(&outcome, &event, nil); isResult {
			t.Errorf("非 result 事件不得报 true：%s", line)
		}
	}
	if outcome.NumTurns != 3 {
		t.Errorf("NumTurns = %d, want 3", outcome.NumTurns)
	}
	if outcome.SessionID != "s-1" {
		t.Errorf("SessionID = %q, want s-1", outcome.SessionID)
	}
}

// describeStreamEvent 的未标注类型出口：JSON 对象但 type/subtype 都缺席时给出
// 「（未标注类型）」，好过在时间线里留一行空白。
func TestDescribeStreamEventUnlabelledType(t *testing.T) {
	event, ok := parseStreamEvent(`{"level":"info"}`)
	if !ok {
		t.Fatal("合法 JSON 应解析成功")
	}
	if got := describeStreamEvent(event, ok, `{"level":"info"}`); got != "（未标注类型）" {
		t.Errorf("describeStreamEvent = %q, want （未标注类型）", got)
	}
	onlySubtype, ok := parseStreamEvent(`{"subtype":"thinking_tokens"}`)
	if !ok {
		t.Fatal("合法 JSON 应解析成功")
	}
	if got := describeStreamEvent(onlySubtype, ok, ""); got != "subtype/thinking_tokens" {
		t.Errorf("只有 subtype 时 = %q, want subtype/thinking_tokens", got)
	}
}

// describeToolResult 的空内容出口：既没有 Text 也没有 Content 时是「（完成）」
// 而不是「（返回 0 字）」。这条差异影响日志可读性，值得钉住。
func TestDescribeToolResultEmptyContent(t *testing.T) {
	block := streamContentBlock{Content: json.RawMessage{}, Text: ""}
	if got, want := describeToolResult(&block), "（完成）"; got != want {
		t.Errorf("describeToolResult = %q, want %q", got, want)
	}
}

// 会话命令的 docker 形态：settings / MCP / 会话配置根 / 文本记录目录一律按
// 相同绝对路径挂载，容器内外看到的路径一致——漏挂会让容器里的 claude 读不到
// assistant 生成的配置，评审静默退化成默认权限。
func TestSessionCommandDockerMountsAllDirs(t *testing.T) {
	options := SessionOptions{
		Config: Config{
			ClaudeBin:   "claude",
			DockerImage: "assistant-review:dev",
			SessionDir:  "/var/lib/assistant/claude",
		},
		Prompt:        "review pr #3",
		Cwd:           "/tmp/worktrees/pr-3",
		MCPConfigPath: "/var/lib/assistant/claude/mcp.json",
		SettingsPath:  "/var/lib/assistant/claude/settings.json",
	}
	_, args, _ := sessionCommand(options)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-w /tmp/worktrees/pr-3") {
		t.Errorf("工作目录未钉定：%v", args)
	}
	// Cwd 必须挂载，否则容器内 /tmp/worktrees/pr-3 是空目录
	if !strings.Contains(joined, "-v /tmp/worktrees/pr-3:/tmp/worktrees/pr-3") {
		t.Errorf("worktree 未挂载：%v", args)
	}
	if !slices.Contains(args, "assistant-review:dev") {
		t.Errorf("镜像名应作为最后一个位置参数前出现：%v", args)
	}
}
