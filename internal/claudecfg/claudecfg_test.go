package claudecfg

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// provider 覆盖合并：env 逐键（provider 优先）、settings 顶层覆盖、permissions
// 逐键合并（allow 并集）、extraAllow 追加。
func TestSessionSettingsWithOverrides(t *testing.T) {
	settings := SessionSettingsMap(Overrides{
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":      "https://gw.example.com",
			"BASH_DEFAULT_TIMEOUT_MS": "900000",
		},
		Settings: map[string]any{
			"model": "glm-4.6",
			"env":   map[string]any{"API_TIMEOUT_MS": "600000"},
			"permissions": map[string]any{
				"allow": []any{"mcp__gateway__*"},
				"deny":  []any{"Bash(rm:*)"},
			},
		},
	}, "mcp__daemon")

	env, _ := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://gw.example.com" {
		t.Errorf("provider env 未注入：%+v", env)
	}
	if env["BASH_DEFAULT_TIMEOUT_MS"] != "900000" {
		t.Errorf("provider env 应覆盖托管默认：%+v", env)
	}
	if env["MAX_MCP_OUTPUT_TOKENS"] != Env["MAX_MCP_OUTPUT_TOKENS"] {
		t.Errorf("托管 env 丢失：%+v", env)
	}
	if env["API_TIMEOUT_MS"] != "600000" {
		t.Errorf("settings.env 未合并：%+v", env)
	}
	if settings["model"] != "glm-4.6" {
		t.Errorf("settings 顶层键未合并：%v", settings["model"])
	}
	permissions, _ := settings["permissions"].(map[string]any)
	allow := reflect.ValueOf(permissions["allow"])
	allowed := make([]string, 0, allow.Len())
	for index := 0; index < allow.Len(); index++ {
		allowed = append(allowed, allow.Index(index).String())
	}
	for _, want := range []string{"mcp__gitea__*", "Bash(git diff:*)", "mcp__gateway__*", "mcp__daemon"} {
		found := false
		for _, value := range allowed {
			if value == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allow 缺 %q：%v", want, allowed)
		}
	}
	if _, ok := permissions["deny"]; !ok {
		t.Errorf("permissions 其他键应合并：%+v", permissions)
	}
	if settings["enableAllProjectMcpServers"] != false {
		t.Errorf("enableAllProjectMcpServers = %v", settings["enableAllProjectMcpServers"])
	}

	// 序列化仍可用
	encoded, err := SessionSettingsWith(Overrides{})
	if err != nil || len(encoded) == 0 {
		t.Fatalf("SessionSettingsWith: %v", err)
	}
}

// MCP 合并：同名 provider 覆盖、stdio server 缺省继承 provider env（自身优先）、
// 非 stdio server 不注入 env、原始 provider 定义不被就地修改。
func TestMergeMCPServers(t *testing.T) {
	overrides := Overrides{
		Env: map[string]string{"API_KEY": "secret", "REGION": "cn"},
		MCP: map[string]any{
			"search": map[string]any{"command": "npx", "args": []any{"-y", "search-mcp"}},
			"custom": map[string]any{
				"command": "custom-mcp",
				"env":     map[string]any{"API_KEY": "own-key"},
			},
			"remote": map[string]any{"type": "http", "url": "https://mcp.example.com"},
			"gitea":  map[string]any{"command": "provider-gitea"},
		},
	}
	base := map[string]any{
		"mcpServers": map[string]any{
			"gitea": map[string]any{"command": "assistant", "args": []any{"mcp", "gitea"}},
		},
	}
	document := MergeMCPServers(base, overrides)
	servers, _ := document["mcpServers"].(map[string]any)
	if len(servers) != 4 {
		t.Fatalf("servers = %+v", servers)
	}
	search, _ := servers["search"].(map[string]any)
	searchEnv, _ := search["env"].(map[string]any)
	if searchEnv["API_KEY"] != "secret" || searchEnv["REGION"] != "cn" {
		t.Errorf("stdio server 应继承 provider env：%+v", searchEnv)
	}
	custom, _ := servers["custom"].(map[string]any)
	customEnv, _ := custom["env"].(map[string]any)
	if customEnv["API_KEY"] != "own-key" || customEnv["REGION"] != "cn" {
		t.Errorf("server 自身 env 优先：%+v", customEnv)
	}
	remote, _ := servers["remote"].(map[string]any)
	if _, ok := remote["env"]; ok {
		t.Errorf("非 stdio server 不应注入 env：%+v", remote)
	}
	if gitea, _ := servers["gitea"].(map[string]any); gitea["command"] != "provider-gitea" {
		t.Errorf("同名 server 应由 provider 覆盖：%+v", gitea)
	}

	// 原始定义不被污染：再合并一次结果一致，且 provider 定义的 search 仍无 env
	again := MergeMCPServers(map[string]any{}, overrides)
	againServers, _ := again["mcpServers"].(map[string]any)
	if !reflect.DeepEqual(againServers, servers) {
		encodedOnce, _ := json.Marshal(servers)
		encodedTwice, _ := json.Marshal(againServers)
		t.Errorf("重复合并结果漂移：\n%s\n%s", encodedOnce, encodedTwice)
	}
	rawSearch, _ := overrides.MCP["search"].(map[string]any)
	if _, ok := rawSearch["env"]; ok {
		t.Errorf("provider 原始 map 被就地修改：%+v", rawSearch)
	}

	// 无 provider mcp 时原样返回 base；base 为 nil 时新建文档
	if empty := MergeMCPServers(base, Overrides{Env: map[string]string{"A": "b"}}); !reflect.DeepEqual(empty, base) {
		t.Errorf("空 mcp 覆盖不应改变 base：%+v", empty)
	}
	created := MergeMCPServers(nil, Overrides{MCP: map[string]any{"only": map[string]any{"command": "x"}}})
	if _, ok := created["mcpServers"].(map[string]any)["only"]; !ok {
		t.Errorf("nil base 应新建 mcpServers：%+v", created)
	}
}

// 两层覆盖叠加（provider 覆盖全局优化点）：env 逐键、settings 浅合并、mcp 同名。
func TestComposeOverrides(t *testing.T) {
	low := Overrides{
		Env:      map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "high", "BASH_MAX_TIMEOUT_MS": "600000"},
		Settings: map[string]any{"model": "global-model", "permissions": map[string]any{"allow": []any{"Bash(global:*)"}}},
		MCP:      map[string]any{"search": map[string]any{"command": "global-search"}},
	}
	high := Overrides{
		Env:      map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example.com", "CLAUDE_CODE_EFFORT_LEVEL": "max"},
		Settings: map[string]any{"apiKeyHelper": "/bin/echo key"},
		MCP:      map[string]any{"search": map[string]any{"command": "provider-search"}},
	}
	composed := ComposeOverrides(low, high)
	if composed.Env["BASH_MAX_TIMEOUT_MS"] != "600000" || composed.Env["ANTHROPIC_BASE_URL"] != "https://gw.example.com" {
		t.Errorf("Env = %+v", composed.Env)
	}
	if composed.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "max" {
		t.Errorf("provider 应覆盖全局同名 env：%+v", composed.Env)
	}
	if composed.Settings["model"] != "global-model" || composed.Settings["apiKeyHelper"] != "/bin/echo key" {
		t.Errorf("Settings = %+v", composed.Settings)
	}
	permissions, _ := composed.Settings["permissions"].(map[string]any)
	if allow, _ := permissions["allow"].([]string); !reflect.DeepEqual(allow, []string{"Bash(global:*)"}) {
		t.Errorf("全局 permissions 应保留：%+v", permissions["allow"])
	}
	search, _ := composed.MCP["search"].(map[string]any)
	if search["command"] != "provider-search" {
		t.Errorf("provider 应覆盖同名 mcp server：%+v", search)
	}
	// 空层直接返回另一层（含指针相等，避免无谓复制）
	if got := ComposeOverrides(Overrides{}, high); !reflect.DeepEqual(got, high) {
		t.Errorf("空 low 应返回 high：%+v", got)
	}
	if got := ComposeOverrides(low, Overrides{}); !reflect.DeepEqual(got, low) {
		t.Errorf("空 high 应返回 low：%+v", got)
	}
	// 合并后的 MCP 去注入：provider env 注入 server，但原始定义不被污染
	merged := MergeMCPServers(nil, composed)
	servers, _ := merged["mcpServers"].(map[string]any)
	searchServer, _ := servers["search"].(map[string]any)
	searchEnv, _ := searchServer["env"].(map[string]any)
	if searchEnv["ANTHROPIC_BASE_URL"] != "https://gw.example.com" || searchEnv["CLAUDE_CODE_EFFORT_LEVEL"] != "max" {
		t.Errorf("合并 env 应注入 server：%+v", searchEnv)
	}
	rawSearch, _ := high.MCP["search"].(map[string]any)
	if _, ok := rawSearch["env"]; ok {
		t.Errorf("原始定义被污染：%+v", rawSearch)
	}
}

// 会话记录位置与项目目录名：官方字符集/长度约束、稳定派生。
func TestSessionLayoutHelpers(t *testing.T) {
	if got := ProjectDirName("assistant-https://gitea.example.com:3000/Ge/assistant"); got != "assistant-https---gitea-example-com-3000-Ge-assistant" {
		t.Errorf("ProjectDirName() = %q", got)
	}
	if got := ProjectDirName("  "); got != "assistant" {
		t.Errorf("空名应回退 assistant：%q", got)
	}
	long := strings.Repeat("a", 80)
	got := ProjectDirName(long)
	if len(got) != 64 || got[:55] != strings.Repeat("a", 55) || got[55] != '-' {
		t.Errorf("超长名应为 55 字符 + - + 8 位哈希（共 64）：%q（len %d）", got, len(got))
	}
	if strings.ContainsAny(got, " /:.") {
		t.Errorf("非法字符未折叠：%q", got)
	}
	// 同名稳定、异名不同
	if ProjectDirName(long) != got {
		t.Error("ProjectDirName 应稳定")
	}

	path := TranscriptPath("/home/ge/.claude", "assistant-ge-assistant", "11111111-2222-4333-8444-555555555555")
	want := filepath.Join("/home/ge/.claude", "projects", "assistant-ge-assistant", "11111111-2222-4333-8444-555555555555.jsonl")
	if path != want {
		t.Errorf("TranscriptPath() = %q, want %q", path, want)
	}
	if TranscriptPath("", "p", "s") != "" || TranscriptPath("/c", "", "s") != "" {
		t.Error("缺项应返回空路径")
	}

	env := SessionEnv("/home/ge/.claude", "assistant-ge-assistant")
	if len(env) != 2 || env[0] != "CLAUDE_CONFIG_DIR=/home/ge/.claude" ||
		env[1] != "CLAUDE_CODE_PROJECT_DIR_NAME=assistant-ge-assistant" {
		t.Errorf("SessionEnv() = %v", env)
	}
	if len(SessionEnv("", "p")) != 0 {
		t.Error("缺 CLAUDE_CONFIG_DIR 时不应设置（官方会忽略项目名）")
	}

	settings := SessionSettingsMap(Overrides{})
	if settings["cleanupPeriodDays"] != CleanupPeriodDays {
		t.Errorf("cleanupPeriodDays = %v, want %d", settings["cleanupPeriodDays"], CleanupPeriodDays)
	}
	// provider 覆盖可改保留期
	custom := SessionSettingsMap(Overrides{Settings: map[string]any{"cleanupPeriodDays": 30}})
	if custom["cleanupPeriodDays"] != 30 {
		t.Errorf("provider 覆盖未生效：%v", custom["cleanupPeriodDays"])
	}
}
