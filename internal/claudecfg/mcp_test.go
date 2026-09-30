package claudecfg

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// assistant 自带的三个 MCP server 定义是会话工具面的契约：会话里能用哪些
// assistant 能力由这三条决定，命令与子命令写错就等于该能力消失。
func TestAssistantMCPServers(t *testing.T) {
	const command = "/usr/local/bin/assistant"
	for _, test := range []struct {
		name     string
		build    func(string) map[string]any
		wantArgs []string
	}{
		{name: "gitea", build: GiteaMCPServer, wantArgs: []string{"mcp", "gitea"}},
		{name: "daemon", build: DaemonMCPServer, wantArgs: []string{"mcp", "daemon"}},
		{name: "sessions", build: SessionsMCPServer, wantArgs: []string{"mcp", "sessions"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := test.build(command)
			if server["command"] != command {
				t.Errorf("command = %v, want %q", server["command"], command)
			}
			args, ok := server["args"].([]any)
			if !ok {
				t.Fatalf("args = %#v, want []any", server["args"])
			}
			got := make([]string, 0, len(args))
			for _, arg := range args {
				got = append(got, arg.(string))
			}
			if !slices.Equal(got, test.wantArgs) {
				t.Errorf("args = %v, want %v", got, test.wantArgs)
			}
		})
	}
}

// MCPServerDef：无参 server 的 args 必须是空数组而非 nil（JSON 里要编码成
// []，写进 mcpServers 后 claude 才认这是一条完整定义）。
func TestMCPServerDef(t *testing.T) {
	server := MCPServerDef("cmd")
	args, ok := server["args"].([]any)
	if !ok || args == nil || len(args) != 0 {
		t.Errorf("args = %#v, want 非 nil 空数组", server["args"])
	}

	server = MCPServerDef("cmd", "a", "b")
	if got := server["args"].([]any); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("args = %v, want [a b]", server["args"])
	}
}

// AssistantCommand 必须给出绝对路径：会话里的 assistant MCP 靠它启动，相对路径
// 会随会话的工作目录漂移。
func TestAssistantCommandIsAbsolute(t *testing.T) {
	command := AssistantCommand()
	if command == "" {
		t.Fatal("AssistantCommand() = 空，want 可执行文件路径")
	}
	if !filepath.IsAbs(command) {
		// 取不到自身路径时回退 PATH 名字，这是唯一允许的非绝对形态
		if command != "assistant" {
			t.Errorf("AssistantCommand() = %q, want 绝对路径或回退名 assistant", command)
		}
	}
	if _, err := os.Stat(command); err != nil && command != "assistant" {
		t.Errorf("AssistantCommand() = %q, 该路径不可访问：%v", command, err)
	}
	// sync.Once：多次调用必须稳定（会话间不漂移）
	if again := AssistantCommand(); again != command {
		t.Errorf("第二次调用 = %q, want %q（应记忆化）", again, command)
	}
}

// ConfigDirEnv 只钉 CLAUDE_CONFIG_DIR：钉上项目目录名会让用户在该目录
// `claude --continue` 接不上会话，所以这里刻意不设。
func TestConfigDirEnv(t *testing.T) {
	got := ConfigDirEnv("/tmp/cfg")
	if !slices.Equal(got, []string{"CLAUDE_CONFIG_DIR=/tmp/cfg"}) {
		t.Errorf("ConfigDirEnv() = %v, want 只含 CLAUDE_CONFIG_DIR", got)
	}
	for _, blank := range []string{"", "   ", "\t"} {
		if got := ConfigDirEnv(blank); got != nil {
			t.Errorf("ConfigDirEnv(%q) = %v, want nil", blank, got)
		}
	}
}

// SessionEnv 两项缺一不可：缺 CLAUDE_CODE_PROJECT_DIR_NAME 记录会随 worktree
// 路径漂移，缺 CLAUDE_CONFIG_DIR 则落错根。任一项缺失即整组返回 nil。
func TestSessionEnv(t *testing.T) {
	got := SessionEnv("/tmp/cfg", "proj")
	want := []string{"CLAUDE_CONFIG_DIR=/tmp/cfg", "CLAUDE_CODE_PROJECT_DIR_NAME=proj"}
	if !slices.Equal(got, want) {
		t.Errorf("SessionEnv() = %v, want %v", got, want)
	}
	for _, test := range []struct{ cfg, project string }{
		{cfg: "", project: "proj"},
		{cfg: "/tmp/cfg", project: ""},
		{cfg: "  ", project: "proj"},
		{cfg: "/tmp/cfg", project: "  "},
		{cfg: "", project: ""},
	} {
		if got := SessionEnv(test.cfg, test.project); got != nil {
			t.Errorf("SessionEnv(%q, %q) = %v, want nil", test.cfg, test.project, got)
		}
	}
}

// Overrides.Counts 是日志与状态展示的唯一口径，三项必须各自独立计数。
func TestOverridesCounts(t *testing.T) {
	empty := Overrides{}
	if env, settings, mcp := empty.Counts(); env != 0 || settings != 0 || mcp != 0 {
		t.Errorf("空覆盖 Counts() = %d/%d/%d, want 0/0/0", env, settings, mcp)
	}

	// 三项取互不相同的数量：相同数量会让「把 settings 与 mcp 弄反」的缺陷
	// 恰好通过测试。
	full := Overrides{
		Env:      map[string]string{"A": "1", "B": "2"},
		Settings: map[string]any{"model": "x", "effort": "high", "tools": nil},
		MCP:      map[string]any{"srv": map[string]any{}},
	}
	if env, settings, mcp := full.Counts(); env != 2 || settings != 3 || mcp != 1 {
		t.Errorf("Counts() = %d/%d/%d, want 2/3/1", env, settings, mcp)
	}
}

// SessionSettings 是无覆盖的便捷形态，必须与 SessionSettingsWith(Overrides{})
// 逐字节一致（否则「默认配置」会有两份真相）。
func TestSessionSettingsEqualsEmptyOverrides(t *testing.T) {
	plain, err := SessionSettings()
	if err != nil {
		t.Fatalf("SessionSettings() error = %v", err)
	}
	withEmpty, err := SessionSettingsWith(Overrides{})
	if err != nil {
		t.Fatalf("SessionSettingsWith() error = %v", err)
	}
	if string(plain) != string(withEmpty) {
		t.Errorf("两者应逐字节一致：\n%s\nvs\n%s", plain, withEmpty)
	}
	if len(plain) == 0 || plain[len(plain)-1] != '\n' {
		t.Error("SessionSettings() 应以换行结尾（写文件的可读形态）")
	}
}

// asDocument 的三种形态：对象原样、map[string]string 转对象、其余折成空对象。
func TestAsDocumentBranches(t *testing.T) {
	object := map[string]any{"a": 1}
	if got := asDocument(object); len(got) != 1 || got["a"] != 1 {
		t.Errorf("asDocument(map[string]any) = %v, want 原样", got)
	}
	if got := asDocument(map[string]string{"k": "v"}); got["k"] != "v" {
		t.Errorf("asDocument(map[string]string) = %v, want 转换后的对象", got)
	}
	for _, value := range []any{nil, "text", 42, []string{"a"}} {
		got := asDocument(value)
		if got == nil || len(got) != 0 {
			t.Errorf("asDocument(%#v) = %v, want 非 nil 空对象", value, got)
		}
	}
}

// cloneDocument 必须深拷贝：provider 定义会被多个会话复用，共享内层 map 会让
// 一个会话的合并污染下一个会话的配置。
func TestCloneDocumentIsDeep(t *testing.T) {
	original := map[string]any{
		"mcpServers": map[string]any{
			"srv": map[string]any{"command": "x", "args": []any{"a"}},
		},
	}
	clone := cloneDocument(original)
	clone["mcpServers"].(map[string]any)["srv"].(map[string]any)["command"] = "mutated"
	if original["mcpServers"].(map[string]any)["srv"].(map[string]any)["command"] != "x" {
		t.Error("修改副本污染了原始文档：cloneDocument 不是深拷贝")
	}
	// nil / 空文档不 panic
	if got := cloneDocument(nil); got != nil {
		t.Errorf("cloneDocument(nil) = %v, want nil", got)
	}
}

// appendAllow 去重追加：新的排后面，已有的不重复。
func TestAppendAllowDeduplicates(t *testing.T) {
	got := appendAllow([]string{"a"}, "b", "a", "c", "b")
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("appendAllow() = %v, want 去重且保序", got)
	}
	// 无追加项时原样返回（不新建切片的行为不影响契约，但内容必须不变）
	if got := appendAllow([]string{"a", "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("appendAllow() = %v, want 原值", got)
	}
	if got := appendAllow(nil, "x"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("appendAllow(nil) = %v, want [x]", got)
	}
}

// stringList 的形态：[]any 只取字符串元素、[]string 复制、单字符串成单元素、
// 其余为 nil。复制语义重要——调用方改自己的切片不能改到已解析的结果。
func TestStringListBranches(t *testing.T) {
	if got := stringList([]any{"a", 1, "b", nil}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("stringList([]any) = %v, want [a b]", got)
	}
	original := []string{"x", "y"}
	copied := stringList(original)
	copied[0] = "mutated"
	if original[0] != "x" {
		t.Error("stringList([]string) 未复制底层数组")
	}
	if got := stringList("solo"); !slices.Equal(got, []string{"solo"}) {
		t.Errorf("stringList(string) = %v, want [solo]", got)
	}
	for _, value := range []any{nil, 42, map[string]any{}} {
		if got := stringList(value); got != nil {
			t.Errorf("stringList(%#v) = %v, want nil", value, got)
		}
	}
}

// resetAssistantCommand 复位缓存以便覆盖各分支（sync.Once 定值后无法再验其它路径）。
// 仅供测试：在生产路径上没有调用点。
func resetAssistantCommand() {
	assistantCommandOnce = sync.Once{}
	assistantCommand = ""
}

// 取不到自身路径（os.Executable 报错）时回退 PATH 名 "assistant"：会话里的 MCP
// 仍能靠 PATH 启动，不至于因为拿不到绝对路径就完全起不来。
func TestAssistantCommandFallsBackWhenExecutableUnavailable(t *testing.T) {
	original := AssistantExecutable
	t.Cleanup(func() { AssistantExecutable = original; resetAssistantCommand() })

	AssistantExecutable = func() (string, error) { return "", errors.New("no executable") }
	resetAssistantCommand()
	if got := AssistantCommand(); got != "assistant" {
		t.Errorf("AssistantCommand = %q, want 回退名 assistant", got)
	}
}

// 自身路径为空串时同样回退（filepath.Abs("") 给出 cwd，不能用）。
func TestAssistantCommandFallsBackOnEmptyPath(t *testing.T) {
	original := AssistantExecutable
	t.Cleanup(func() { AssistantExecutable = original; resetAssistantCommand() })

	AssistantExecutable = func() (string, error) { return "   ", nil }
	resetAssistantCommand()
	if got := AssistantCommand(); got != "assistant" {
		t.Errorf("AssistantCommand = %q, want 回退名 assistant", got)
	}
}

// 拿到相对路径时给出绝对路径（会话可能在任何工作目录里启动）。
func TestAssistantCommandAbsolutizesRelativePath(t *testing.T) {
	original := AssistantExecutable
	t.Cleanup(func() { AssistantExecutable = original; resetAssistantCommand() })

	AssistantExecutable = func() (string, error) { return "bin/assistant", nil }
	resetAssistantCommand()
	got := AssistantCommand()
	if !filepath.IsAbs(got) {
		t.Errorf("AssistantCommand = %q, want 绝对路径", got)
	}
	if !strings.HasSuffix(got, filepath.Join("bin", "assistant")) {
		t.Errorf("AssistantCommand = %q, 应保留原始尾部", got)
	}
}

// ASSISTANT_CLI 显式覆盖优先于一切推断：两个二进制装在不同前缀时用它指路。
func TestAssistantCommandHonorsExplicitOverride(t *testing.T) {
	original := AssistantExecutable
	t.Cleanup(func() { AssistantExecutable = original; resetAssistantCommand() })

	AssistantExecutable = func() (string, error) { return "/data/assistantd/bin/assistantd", nil }
	t.Setenv("ASSISTANT_CLI", "/opt/assistant/bin/assistant")
	resetAssistantCommand()
	if got := AssistantCommand(); got != "/opt/assistant/bin/assistant" {
		t.Errorf("AssistantCommand = %q, want ASSISTANT_CLI 覆盖值", got)
	}
}

// 自身不是 assistant 时优先同目录的 assistant：两个二进制装在同一个前缀下。
func TestAssistantCommandPrefersSiblingAssistant(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "assistant")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := AssistantExecutable
	t.Cleanup(func() { AssistantExecutable = original; resetAssistantCommand() })

	AssistantExecutable = func() (string, error) { return filepath.Join(dir, "assistantd"), nil }
	resetAssistantCommand()
	if got := AssistantCommand(); got != sibling {
		t.Errorf("AssistantCommand = %q, want 同目录 assistant %q", got, sibling)
	}
}

// 自身不是 assistant、同目录也没有 assistant（如 go run / 测试二进制）时回退
// PATH 名 assistant。
func TestAssistantCommandFallsBackWithoutSibling(t *testing.T) {
	dir := t.TempDir()
	original := AssistantExecutable
	t.Cleanup(func() { AssistantExecutable = original; resetAssistantCommand() })

	AssistantExecutable = func() (string, error) { return filepath.Join(dir, "assistantd"), nil }
	resetAssistantCommand()
	if got := AssistantCommand(); got != "assistant" {
		t.Errorf("AssistantCommand = %q, want 回退名 assistant", got)
	}
}
