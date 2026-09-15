package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/claudecfg"
)

// 对话会话按配置走 --bare（最小模式），并把自举 MCP 之外的上下文钉死为显式参数。
func TestChatBareArgs(t *testing.T) {
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		Bare:       true,
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if !chat.Bare() {
		t.Fatal("Bare() 应为 true")
	}
	args, err := chat.sessionArgs("11111111-2222-4333-8444-555555555555", "chat-test", true, "你好", t.TempDir())
	if err != nil {
		t.Fatalf("sessionArgs: %v", err)
	}
	for _, want := range []string{"--bare", "--strict-mcp-config", "--settings", "--append-system-prompt"} {
		if !contains(args, want) {
			t.Errorf("缺少 %s：%v", want, args)
		}
	}
}

// 关闭最小模式时不下发 --bare（老 claude 退回普通模式）。
func TestChatWithoutBareArgs(t *testing.T) {
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	args, err := chat.sessionArgs("11111111-2222-4333-8444-555555555555", "", true, "你好", t.TempDir())
	if err != nil {
		t.Fatalf("sessionArgs: %v", err)
	}
	if contains(args, "--bare") {
		t.Errorf("未开启最小模式时不该下发 --bare：%v", args)
	}
}

// 对话会话的 MCP 由 assistant 自己提供（绝对路径，不依赖 PATH 或用户配置），
// provider 原生 server 合并进来。
func TestChatMCPInjectsAssistantCommand(t *testing.T) {
	stateDir := t.TempDir()
	chat, err := NewChat(ChatConfig{StateDir: stateDir, SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	// settings/mcp 落会话自己的工作目录：并发会话各写各的，互不覆盖
	workspaceDir, err := chat.WorkspaceDir("user-1")
	if err != nil {
		t.Fatalf("WorkspaceDir: %v", err)
	}
	path, err := chat.writeMCPConfig(claudecfg.Overrides{
		MCP: map[string]any{"search": map[string]any{"command": "search-mcp"}},
	}, workspaceDir)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if filepath.Dir(path) != workspaceDir {
		t.Errorf("MCP 配置应落在会话工作目录 %s：%s", workspaceDir, path)
	}
	if !strings.HasPrefix(workspaceDir, stateDir) {
		t.Errorf("工作目录应在状态目录内：%s", workspaceDir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	servers, _ := document["mcpServers"].(map[string]any)
	daemonServer, _ := servers[claudecfg.MCPServerDaemon].(map[string]any)
	if daemonServer["command"] != claudecfg.AssistantCommand() {
		t.Errorf("daemon MCP 应由 assistant 自己提供：%+v", daemonServer)
	}
	if args, _ := daemonServer["args"].([]any); len(args) != 2 || args[0] != "mcp" || args[1] != "daemon" {
		t.Errorf("daemon MCP args 不对：%+v", daemonServer)
	}
	if _, ok := servers["search"]; !ok {
		t.Errorf("provider MCP 应合并：%+v", servers)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

// 每个微信会话一个稳定工作目录：同一会话跨轮不变、不同会话互不串；工作目录里
// 留下 session.json（会话 id / 模型 / 续聊路径），这就是「持久化在一个目录中」。
func TestChatWorkspaceStableAndIsolated(t *testing.T) {
	stateDir := t.TempDir()
	var dirs []string
	chat, err := NewChat(ChatConfig{
		StateDir:   stateDir,
		SessionDir: t.TempDir(),
		RunClaude: func(_ context.Context, _ string, _ []string, dir string, _ []string) ([]byte, error) {
			dirs = append(dirs, dir)
			return []byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	ctx := context.Background()
	for _, text := range []string{"第一条", "第二条"} {
		if _, err := chat.Handle(ctx, "user-1", text); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := chat.Handle(ctx, "user-2", "你好"); err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 3 || dirs[0] != dirs[1] {
		t.Fatalf("同一会话的工作目录应稳定：%v", dirs)
	}
	if dirs[0] == dirs[2] {
		t.Errorf("不同会话的工作目录应隔离：%v", dirs)
	}
	if !strings.HasPrefix(dirs[0], stateDir) {
		t.Errorf("工作目录应在状态目录内：%s", dirs[0])
	}

	metadata, err := readSessionMetadata(dirs[0])
	if err != nil {
		t.Fatalf("读取 session.json: %v", err)
	}
	if metadata.SessionID == "" || metadata.Workdir != dirs[0] || metadata.ConversationID != "user-1" {
		t.Errorf("session.json = %+v", metadata)
	}
	if metadata.Title != chatSessionTitle("user-1") || metadata.CreatedAt == "" {
		t.Errorf("session.json 缺标题/创建时间：%+v", metadata)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "settings.json")); err != nil {
		t.Errorf("会话设置应落在工作目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "mcp.json")); err != nil {
		t.Errorf("会话 MCP 配置应落在工作目录：%v", err)
	}
}

// 用户明确「重新开始」：下一条消息用新的 --session-id，而不是 --resume 老会话。
func TestChatResetStartsFreshSession(t *testing.T) {
	var args [][]string
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		RunClaude: func(_ context.Context, _ string, runArgs []string, _ string, _ []string) ([]byte, error) {
			args = append(args, args2copy(runArgs))
			return []byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	ctx := context.Background()
	if _, err := chat.Handle(ctx, "user-1", "第一句"); err != nil {
		t.Fatal(err)
	}
	first := argumentAfter(args[0], "--session-id")
	if first == "" {
		t.Fatalf("首轮缺 --session-id：%v", args[0])
	}
	if err := chat.Reset("user-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := chat.Handle(ctx, "user-1", "重新开始后第一句"); err != nil {
		t.Fatal(err)
	}
	second := argumentAfter(args[1], "--session-id")
	if second == "" || second == first {
		t.Errorf("重置后应是新会话：first=%q second=%q args=%v", first, second, args[1])
	}
	if argumentAfter(args[1], "--resume") != "" {
		t.Errorf("重置后不该 --resume 老会话：%v", args[1])
	}
}

// args2copy 复制参数切片（注入的 RunClaude 会被复用同一个底层数组）。
func args2copy(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	return out
}

// 「重新开始」的触发词：斜杠命令与直白说法都认，普通聊天不受影响。
func TestRestartRequested(t *testing.T) {
	for _, text := range []string{"/new", " /reset ", "重新开始", "新会话", "/clear"} {
		if !restartRequested(text) {
			t.Errorf("%q 应触发重新开始", text)
		}
	}
	for _, text := range []string{"继续", "new", "重新开始吧，但先回答我", "/news"} {
		if restartRequested(text) {
			t.Errorf("%q 不该触发重新开始", text)
		}
	}
}

// 早期版本把所有会话共用的 settings.json / mcp.json 放在 StateDir 根：新布局下
// 它们已无人读取，启动时清掉，避免和会话工作目录里的同名文件混淆。
func TestChatDropsLegacySharedConfig(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"settings.json", "mcp.json", "sessions.json"} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewChat(ChatConfig{StateDir: stateDir, SessionDir: t.TempDir()}); err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	for _, name := range []string{"settings.json", "mcp.json"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s 应被清理：%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "sessions.json")); err != nil {
		t.Errorf("会话映射不该被动：%v", err)
	}
}
