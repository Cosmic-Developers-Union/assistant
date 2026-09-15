package claudecfg

import (
	"strings"
	"testing"
)

// 密钥打码：留首尾便于核对是哪一把，太短的整段打码。
func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"short":                "***",
		"123456789012":         "***",
		"sk-cp-abcdefghijklmn": "sk-cp-…klmn",
	}
	for input, want := range cases {
		if got := MaskSecret(input); got != want {
			t.Errorf("MaskSecret(%q) = %q, want %q", input, got, want)
		}
	}
}

// 只对密钥类变量打码：端点/模型/超时都是调试要看的，必须原样出现。
func TestMaskEnvValue(t *testing.T) {
	if got := MaskEnvValue("ANTHROPIC_AUTH_TOKEN", "sk-cp-abcdefghijklmn"); !strings.Contains(got, "…") {
		t.Errorf("密钥应打码：%q", got)
	}
	if got := MaskEnvValue("ANTHROPIC_BASE_URL", "https://api.minimaxi.com/anthropic"); got != "https://api.minimaxi.com/anthropic" {
		t.Errorf("端点不该打码：%q", got)
	}
	if got := MaskEnvValue("ANTHROPIC_MODEL", "MiniMax-M3[1m]"); got != "MiniMax-M3[1m]" {
		t.Errorf("模型名不该打码：%q", got)
	}
}

// 会话生效覆盖的一行摘要：env 带打码值、settings 只列键名。
func TestOverridesDescribe(t *testing.T) {
	overrides := Overrides{
		Env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "sk-cp-abcdefghijklmn",
			"ANTHROPIC_BASE_URL":   "https://api.minimaxi.com/anthropic",
			"ANTHROPIC_MODEL":      "MiniMax-M3[1m]",
		},
		Settings: map[string]any{"model": "m", "apiKeyHelper": "echo x"},
		MCP:      map[string]any{"MiniMax": map[string]any{"command": "uvx"}},
	}
	got := overrides.Describe()
	for _, want := range []string{
		"env 3 项",
		"ANTHROPIC_AUTH_TOKEN=sk-cp-…klmn",
		"ANTHROPIC_BASE_URL=https://api.minimaxi.com/anthropic",
		"ANTHROPIC_MODEL=MiniMax-M3[1m]",
		"settings 2 项（apiKeyHelper,model，值不打印）",
		"mcp 1 个",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe 缺少 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "echo x") {
		t.Errorf("settings 值不该打印：%s", got)
	}
	if empty := (Overrides{}).Describe(); !strings.Contains(empty, "env 0 项（无）") {
		t.Errorf("空覆盖描述 = %q", empty)
	}
}

// MCP 配置摘要：声明了什么 server、用什么命令、env 里哪些密钥（打码）。
func TestDescribeMCPServers(t *testing.T) {
	document := map[string]any{"mcpServers": map[string]any{
		"daemon": map[string]any{"command": "assistant", "args": []any{"mcp", "daemon"}},
		"MiniMax": map[string]any{
			"command": "uvx",
			"args":    []any{"minimax-coding-plan-mcp"},
			"env":     map[string]any{"MINIMAX_API_KEY": "sk-cp-abcdefghijklmn", "MINIMAX_API_HOST": "https://api.minimax.cn"},
		},
	}}
	got := DescribeMCPServers(document)
	for _, want := range []string{
		"daemon（assistant mcp daemon）",
		"MiniMax（uvx minimax-coding-plan-mcp [MINIMAX_API_HOST=https://api.minimax.cn MINIMAX_API_KEY=sk-cp-…klmn]）",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribeMCPServers 缺少 %q：%s", want, got)
		}
	}
	if DescribeMCPServers(map[string]any{}) != "无" {
		t.Error("空文档应为「无」")
	}
}
