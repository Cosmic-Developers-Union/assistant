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

// 对话会话与评审会话共用 provider 体系：env/settings 注入 --settings，原生
// MCP server 合并进自举 MCP 配置（env 缺省注入）。
func TestChatProviderOverrides(t *testing.T) {
	chat, err := NewChat(ChatConfig{
		ClaudeBin:  "claude",
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		Provider: claudecfg.Overrides{
			Env:      map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example.com"},
			Settings: map[string]any{"model": "glm-4.6"},
			MCP:      map[string]any{"search": map[string]any{"command": "search-mcp"}},
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	settingsPath, err := chat.writeSettings(chat.config.Provider, t.TempDir())
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

	mcpPath, err := chat.writeMCPConfig(chat.config.Provider, t.TempDir())
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
		SessionDir:   t.TempDir(),
		ProviderName: "opencode",
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	sessionID := "11111111-2222-4333-8444-555555555555"
	args, err := chat.sessionArgs(sessionID, "chat-test", true, "你好", t.TempDir(), chat.agentFor("user-1", ""))
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

// 对话会话：稳定标题（--name）+ 固定文本记录项目目录名（跨轮 --resume 同一 ID）。
func TestChatSessionTitleAndProjectEnv(t *testing.T) {
	stateDir := t.TempDir()
	var gotArgs, gotEnv []string
	turns := 0
	chat, err := NewChat(ChatConfig{
		ClaudeBin:  "claude",
		StateDir:   stateDir,
		SessionDir: filepath.Join(stateDir, "claude"),
		RunClaude: func(_ context.Context, _ string, args []string, _ string, env []string) ([]byte, error) {
			gotArgs, gotEnv = args, env
			turns++
			return []byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	conversation := "o9cq80yYcUkby1i0GsUNJ1u00RSM@im.wechat"
	reply, err := chat.Handle(context.Background(), conversation, Turn{Transport: "weixin", Text: "在吗"})
	if err != nil || reply != "好的" {
		t.Fatalf("Handle: reply=%q err=%v", reply, err)
	}
	title := chatSessionTitle(conversation)
	if argumentAfter(gotArgs, "--name") != title {
		t.Errorf("--name = %q, want %q", argumentAfter(gotArgs, "--name"), title)
	}
	if !strings.HasPrefix(title, "chat-") || len(title) != len("chat-")+8 {
		t.Errorf("标题格式 = %q", title)
	}
	firstID := argumentAfter(gotArgs, "--session-id")
	if firstID == "" {
		t.Fatalf("首轮缺 --session-id：%v", gotArgs)
	}
	// 对话会话只钉 CLAUDE_CONFIG_DIR（记录落 assistant 托管配置根）；项目名交给
	// 稳定的会话工作目录自然派生，这样 cd 进去 `claude --continue` 能接上会话
	envJoined := strings.Join(gotEnv, "\n")
	if !strings.Contains(envJoined, "CLAUDE_CONFIG_DIR="+filepath.Join(stateDir, "claude")) {
		t.Errorf("CLAUDE_CONFIG_DIR 未注入：%v", gotEnv)
	}
	if strings.Contains(envJoined, "CLAUDE_CODE_PROJECT_DIR_NAME") {
		t.Errorf("对话会话不该钉 CLAUDE_CODE_PROJECT_DIR_NAME（会破坏 --continue）：%v", gotEnv)
	}
	workspace, err := chat.WorkspaceDir(conversation)
	if err != nil {
		t.Fatalf("WorkspaceDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "session.json")); err != nil {
		t.Errorf("会话工作目录缺少 session.json：%v", err)
	}
	// 第二轮复用同一会话 ID（--resume），标题稳定
	if _, err := chat.Handle(context.Background(), conversation, Turn{Transport: "weixin", Text: "再问一句"}); err != nil {
		t.Fatal(err)
	}
	if got := argumentAfter(gotArgs, "--resume"); got != firstID {
		t.Errorf("--resume = %q, want %q", got, firstID)
	}
	if argumentAfter(gotArgs, "--name") != title {
		t.Errorf("标题应跨轮稳定：%q", argumentAfter(gotArgs, "--name"))
	}
	if turns != 2 {
		t.Errorf("turns = %d", turns)
	}
}
