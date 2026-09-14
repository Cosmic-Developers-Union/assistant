package provider

import (
	"strings"
	"testing"

	"assistant/internal/claudecfg"
)

// 令牌简写按预设落到正确的环境变量，预设默认（端点/超时/模型映射）自动打底。
func TestResolvePresetAndTokenShorthand(t *testing.T) {
	tests := []struct {
		name      string
		tokenEnvs []string
		baseURL   string
	}{
		{"zhipu", []string{"ANTHROPIC_AUTH_TOKEN", "Z_AI_API_KEY"}, "https://api.z.ai/api/anthropic"},
		{"glm", []string{"ANTHROPIC_AUTH_TOKEN", "Z_AI_API_KEY"}, "https://api.z.ai/api/anthropic"},
		{"kimi", []string{"ANTHROPIC_API_KEY"}, "https://api.kimi.com/coding/"},
		{"minimax", []string{"ANTHROPIC_AUTH_TOKEN", "MINIMAX_API_KEY"}, "https://api.minimax.io/anthropic"},
		{"opencode", []string{"ANTHROPIC_AUTH_TOKEN"}, "https://opencode.ai/zen"},
		{"zen", []string{"ANTHROPIC_AUTH_TOKEN"}, "https://opencode.ai/zen"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overrides, err := Resolve(test.name, claudecfg.Overrides{}, claudecfg.Overrides{}, "secret-token")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			for _, tokenEnv := range test.tokenEnvs {
				if overrides.Env[tokenEnv] != "secret-token" {
					t.Errorf("%s = %q, want token（令牌简写应覆盖 MCP 所需变量）", tokenEnv, overrides.Env[tokenEnv])
				}
			}
			if overrides.Env["ANTHROPIC_BASE_URL"] != test.baseURL {
				t.Errorf("base_url = %q, want %q", overrides.Env["ANTHROPIC_BASE_URL"], test.baseURL)
			}
		})
	}
	// Anthropic 官方：令牌进 API_KEY，不设端点
	anthropic, err := Resolve("anthropic", claudecfg.Overrides{}, claudecfg.Overrides{}, "sk-ant-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if anthropic.Env["ANTHROPIC_API_KEY"] != "sk-ant-1" || anthropic.Env["ANTHROPIC_BASE_URL"] != "" {
		t.Errorf("anthropic env = %+v", anthropic.Env)
	}
	// 未识别 provider：无预设，令牌落默认 AUTH_TOKEN
	unknown, err := Resolve("my-gateway", claudecfg.Overrides{}, claudecfg.Overrides{}, "tok")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if unknown.Env["ANTHROPIC_AUTH_TOKEN"] != "tok" || len(unknown.Env) != 1 {
		t.Errorf("未知 provider 不应套预设：%+v", unknown.Env)
	}
}

// 覆盖顺序：预设 < 全局优化点 < 用户 provider；用户空值移除预设默认。
func TestResolvePrecedenceAndRemoval(t *testing.T) {
	global := claudecfg.Overrides{Env: map[string]string{"API_TIMEOUT_MS": "600000", "GLOBAL_ONLY": "1"}}
	user := claudecfg.Overrides{
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":           "https://my-gateway.example.com",
			"ANTHROPIC_MODEL":              "", // 移除预设钉的模型
			"ANTHROPIC_DEFAULT_OPUS_MODEL": "",
		},
	}
	overrides, err := Resolve("zhipu", global, user, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if overrides.Env["ANTHROPIC_BASE_URL"] != "https://my-gateway.example.com" {
		t.Errorf("用户 base_url 未生效：%+v", overrides.Env)
	}
	if overrides.Env["API_TIMEOUT_MS"] != "600000" {
		t.Errorf("全局优化点应覆盖预设：%+v", overrides.Env)
	}
	if overrides.Env["GLOBAL_ONLY"] != "1" {
		t.Errorf("全局优化点丢失：%+v", overrides.Env)
	}
	if _, exists := overrides.Env["ANTHROPIC_MODEL"]; exists {
		t.Errorf("用户空值应移除预设默认：%+v", overrides.Env)
	}
	// kimi 预设钉了模型，空值移除后不应残留任何 DEFAULT_* 映射
	kimi, err := Resolve("kimi", claudecfg.Overrides{}, claudecfg.Overrides{
		Env: map[string]string{"ANTHROPIC_MODEL": ""},
	}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, exists := kimi.Env["ANTHROPIC_MODEL"]; exists {
		t.Errorf("kimi 模型未被移除：%+v", kimi.Env)
	}
}

// openai 没有 Anthropic 端点：缺 base_url 在校验期报错，给出翻译代理指引。
func TestResolveOpenAIRequiresBaseURL(t *testing.T) {
	if _, err := Resolve("openai", claudecfg.Overrides{}, claudecfg.Overrides{}, "sk-x"); err == nil {
		t.Fatal("缺 base_url 应报错")
	} else if !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
		t.Errorf("错误信息应指向 base_url：%v", err)
	}
	overrides, err := Resolve("openai", claudecfg.Overrides{}, claudecfg.Overrides{
		Env: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000"},
	}, "sk-x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if overrides.Env["ANTHROPIC_AUTH_TOKEN"] != "sk-x" {
		t.Errorf("令牌未落地：%+v", overrides.Env)
	}
}

// 预设注册表：主名与别名的可查性、名字集合稳定。
func TestPresetRegistry(t *testing.T) {
	for _, name := range []string{"anthropic", "claude", "zhipu", "glm", "bigmodel", "kimi", "moonshot", "minimax", "minimax-cn", "opencode", "zen", "openai"} {
		if !HasPreset(name) {
			t.Errorf("HasPreset(%q) = false", name)
		}
	}
	if HasPreset("my-gateway") {
		t.Error("未注册名字不应命中预设")
	}
	names := strings.Join(PresetNames(), ",")
	for _, want := range []string{"anthropic", "zhipu", "kimi", "minimax", "opencode", "openai"} {
		if !strings.Contains(names, want) {
			t.Errorf("PresetNames() 缺 %q：%s", want, names)
		}
	}
}

// 供应商官方 MCP 随预设自动注入：zhipu 视觉理解（npx）与 MiniMax coding-plan
// （uvx）；api_key 简写同时写进各自 MCP 需要的环境变量（server env 缺省继承）。
func TestPresetVendorMCP(t *testing.T) {
	zhipu, err := Resolve("zhipu", claudecfg.Overrides{}, claudecfg.Overrides{}, "zai-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	zhipuServer := mcpServer(t, claudecfg.MergeMCPServers(nil, zhipu), "zai-mcp-server")
	if zhipuServer["command"] != "npx" {
		t.Errorf("zhipu MCP command = %v", zhipuServer["command"])
	}
	args, _ := zhipuServer["args"].([]any)
	if len(args) != 2 || args[0] != "-y" || args[1] != "@z_ai/mcp-server@latest" {
		t.Errorf("zhipu MCP args = %v", zhipuServer["args"])
	}
	zhipuEnv, _ := zhipuServer["env"].(map[string]any)
	if zhipuEnv["Z_AI_MODE"] != "ZAI" {
		t.Errorf("zhipu MCP Z_AI_MODE = %v", zhipuEnv["Z_AI_MODE"])
	}
	if zhipuEnv["Z_AI_API_KEY"] != "zai-key" {
		t.Errorf("zhipu MCP 未继承 api_key：%v", zhipuEnv)
	}
	// 全档 glm-5.3-flash[1m] + 1M 压缩窗口（flash 绝对优先）
	for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		if zhipu.Env[key] != "glm-5.3-flash[1m]" {
			t.Errorf("zhipu %s = %q", key, zhipu.Env[key])
		}
	}
	if zhipu.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "1000000" {
		t.Errorf("zhipu 压缩窗口 = %q", zhipu.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"])
	}
	bigmodel, err := Resolve("bigmodel", claudecfg.Overrides{}, claudecfg.Overrides{}, "zai-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	bigmodelServer := mcpServer(t, claudecfg.MergeMCPServers(nil, bigmodel), "zai-mcp-server")
	bigmodelEnv, _ := bigmodelServer["env"].(map[string]any)
	if bigmodelEnv["Z_AI_MODE"] != "ZHIPU" {
		t.Errorf("bigmodel MCP Z_AI_MODE = %v", bigmodelEnv["Z_AI_MODE"])
	}

	minimax, err := Resolve("minimax", claudecfg.Overrides{}, claudecfg.Overrides{}, "sk-cp-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	server := mcpServer(t, claudecfg.MergeMCPServers(nil, minimax), "MiniMax")
	if server["command"] != "uvx" {
		t.Errorf("minimax MCP command = %v", server["command"])
	}
	serverEnv, _ := server["env"].(map[string]any)
	if serverEnv["MINIMAX_API_HOST"] != "https://api.minimax.io" || serverEnv["MINIMAX_API_KEY"] != "sk-cp-1" {
		t.Errorf("minimax MCP env = %v", serverEnv)
	}
	minimaxCN, err := Resolve("minimax-cn", claudecfg.Overrides{}, claudecfg.Overrides{}, "sk-cp-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cnServer := mcpServer(t, claudecfg.MergeMCPServers(nil, minimaxCN), "MiniMax")
	cnEnv, _ := cnServer["env"].(map[string]any)
	if cnEnv["MINIMAX_API_HOST"] != "https://api.minimax.cn" {
		t.Errorf("minimax-cn MCP env = %v", cnEnv)
	}

	// 用户显式配置的 MCP env 优先于注入（server 自身取值优先）
	custom, err := Resolve("minimax", claudecfg.Overrides{}, claudecfg.Overrides{
		MCP: map[string]any{
			"MiniMax": map[string]any{
				"command": "uvx",
				"args":    []any{"minimax-coding-plan-mcp"},
				"env":     map[string]any{"MINIMAX_API_HOST": "https://custom.example.com"},
			},
		},
	}, "sk-cp-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	customServer := mcpServer(t, claudecfg.MergeMCPServers(nil, custom), "MiniMax")
	customEnv, _ := customServer["env"].(map[string]any)
	if customEnv["MINIMAX_API_HOST"] != "https://custom.example.com" || customEnv["MINIMAX_API_KEY"] != "sk-cp-1" {
		t.Errorf("用户 MCP env 应优先且仍注入密钥：%v", customEnv)
	}
}

func mcpServer(t *testing.T, document map[string]any, name string) map[string]any {
	t.Helper()
	servers, _ := document["mcpServers"].(map[string]any)
	server, _ := servers[name].(map[string]any)
	if server == nil {
		t.Fatalf("MCP server %q 缺失：%+v", name, document)
	}
	return server
}

// 预设 MCP 可以关闭：用户 mcp 里把同名 server 设为 null。
func TestPresetMCPRemoval(t *testing.T) {
	overrides, err := Resolve("minimax", claudecfg.Overrides{}, claudecfg.Overrides{
		MCP: map[string]any{"MiniMax": nil},
	}, "sk-cp-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	document := claudecfg.MergeMCPServers(nil, overrides)
	servers, _ := document["mcpServers"].(map[string]any)
	if _, exists := servers["MiniMax"]; exists {
		t.Errorf("null 应移除预设 MCP：%+v", servers)
	}
}
