package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"assistant/internal/claudecfg"
)

// 对话会话与评审会话共用 provider 体系：env/settings 注入 --settings，原生
// MCP server 合并进自举 MCP 配置（env 缺省注入）。
func TestChatProviderOverrides(t *testing.T) {
	chat, err := NewChat(ChatConfig{
		ClaudeBin: "claude",
		StateDir:  t.TempDir(),
		Provider: claudecfg.Overrides{
			Env:      map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example.com"},
			Settings: map[string]any{"model": "glm-4.6"},
			MCP:      map[string]any{"search": map[string]any{"command": "search-mcp"}},
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	settingsPath, err := chat.writeSettings(chat.config.Provider)
	if err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	var settings map[string]any
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	env, _ := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://gw.example.com" {
		t.Errorf("provider env 未注入：%+v", env)
	}
	if settings["model"] != "glm-4.6" {
		t.Errorf("provider settings 未合并：%v", settings["model"])
	}
	permissions, _ := settings["permissions"].(map[string]any)
	allow, _ := permissions["allow"].([]any)
	found := false
	for _, value := range allow {
		if value == "mcp__daemon" {
			found = true
		}
	}
	if !found {
		t.Errorf("daemon MCP 放行丢失：%v", allow)
	}

	mcpPath, err := chat.writeMCPConfig(chat.config.Provider)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	var document map[string]any
	data, err = os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	servers, _ := document["mcpServers"].(map[string]any)
	if _, ok := servers["daemon"]; !ok {
		t.Errorf("自举 daemon server 丢失：%+v", servers)
	}
	search, _ := servers["search"].(map[string]any)
	searchEnv, _ := search["env"].(map[string]any)
	if searchEnv["ANTHROPIC_BASE_URL"] != "https://gw.example.com" {
		t.Errorf("provider env 未注入 MCP：%+v", searchEnv)
	}
}

// provider 名命中代码级特化（opencode）：对话会话 settings 带会话请求头，
// 且复用该会话的稳定 UUID（多轮命中网关缓存）。
func TestChatProviderSessionHeader(t *testing.T) {
	chat, err := NewChat(ChatConfig{
		ClaudeBin:    "claude",
		StateDir:     t.TempDir(),
		ProviderName: "opencode",
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	sessionID := "11111111-2222-4333-8444-555555555555"
	args, err := chat.sessionArgs(sessionID, true, "你好")
	if err != nil {
		t.Fatalf("sessionArgs: %v", err)
	}
	settingsPath := argumentAfter(args, "--settings")
	if settingsPath == "" {
		t.Fatalf("args 缺 --settings：%v", args)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	env, _ := settings["env"].(map[string]any)
	headers, _ := env["ANTHROPIC_CUSTOM_HEADERS"].(string)
	if !strings.Contains(headers, "x-opencode-session: "+sessionID) {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q", headers)
	}
}

func argumentAfter(args []string, flag string) string {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}
