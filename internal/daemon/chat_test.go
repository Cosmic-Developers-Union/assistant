package daemon

import (
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
	args, err := chat.sessionArgs("11111111-2222-4333-8444-555555555555", "chat-test", true, "你好")
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
	args, err := chat.sessionArgs("11111111-2222-4333-8444-555555555555", "", true, "你好")
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
	path, err := chat.writeMCPConfig(claudecfg.Overrides{
		MCP: map[string]any{"search": map[string]any{"command": "search-mcp"}},
	})
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if filepath.Dir(path) != stateDir {
		t.Errorf("MCP 配置应落在会话状态目录：%s", path)
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
