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

// env / settings / MCP 都按行展开：日志一行一个事实，不压成长行。
func TestEnvSettingAndMCPLines(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-cp-abcdefghijklmn",
		"ANTHROPIC_BASE_URL":   "https://api.minimaxi.com/anthropic",
		"ANTHROPIC_MODEL":      "MiniMax-M3[1m]",
	}
	lines := EnvLines(env)
	want := []string{
		"ANTHROPIC_AUTH_TOKEN = sk-cp-…klmn",
		"ANTHROPIC_BASE_URL = https://api.minimaxi.com/anthropic",
		"ANTHROPIC_MODEL = MiniMax-M3[1m]",
	}
	if len(lines) != len(want) {
		t.Fatalf("EnvLines = %v", lines)
	}
	for index, line := range want {
		if lines[index] != line {
			t.Errorf("EnvLines[%d] = %q, want %q", index, lines[index], line)
		}
	}
	if EnvLines(nil)[0] != "（无）" {
		t.Errorf("空 env 应显示（无）：%v", EnvLines(nil))
	}
	if keys := SettingKeys(map[string]any{"model": "m", "apiKeyHelper": "echo x"}); strings.Join(keys, ",") != "apiKeyHelper,model" {
		t.Errorf("SettingKeys = %v", keys)
	}
}

// MCP 配置按行展开：server 一行，其 env 再逐行缩进（密钥打码）。
func TestMCPServerLines(t *testing.T) {
	document := map[string]any{"mcpServers": map[string]any{
		"daemon": map[string]any{"command": "assistant", "args": []any{"mcp", "daemon"}},
		"MiniMax": map[string]any{
			"command": "uvx",
			"args":    []any{"minimax-coding-plan-mcp"},
			"env":     map[string]any{"MINIMAX_API_KEY": "sk-cp-abcdefghijklmn", "MINIMAX_API_HOST": "https://api.minimax.cn"},
		},
	}}
	lines := MCPServerLines(document)
	want := []string{
		"MiniMax：uvx minimax-coding-plan-mcp",
		"  env.MINIMAX_API_HOST = https://api.minimax.cn",
		"  env.MINIMAX_API_KEY = sk-cp-…klmn",
		"daemon：assistant mcp daemon",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("MCPServerLines =\n%s\nwant=\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if MCPServerLines(map[string]any{})[0] != "（无）" {
		t.Error("空文档应为（无）")
	}
}

// 工具入参这类任意 JSON：缩进展开、密钥字段打码、超行数截断。
func TestJSONLines(t *testing.T) {
	lines := JSONLines(map[string]any{
		"title":   "x",
		"api_key": "sk-cp-abcdefghijklmn",
		"nested":  map[string]any{"token": "abcdefghijklmnop"},
	}, 10)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{`"title": "x"`, `"api_key": "sk-cp-…klmn"`, `"token": "abcdef…mnop"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("JSONLines 缺少 %q：\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "\n  ") {
		t.Errorf("应缩进展开：\n%s", joined)
	}
	if truncated := JSONLines([]any{1, 2, 3, 4, 5}, 2); len(truncated) != 3 || !strings.Contains(truncated[2], "已截断") {
		t.Errorf("超行数应截断：%v", truncated)
	}
	if JSONLines(nil, 5) != nil {
		t.Error("空值应返回 nil")
	}
}

// 工具结果这类多行文本：丢空行、按行截断。
func TestTextLines(t *testing.T) {
	lines := TextLines("第一行\n\n第二行\n"+strings.Repeat("x", 300), 5, 20)
	if len(lines) != 3 || lines[0] != "第一行" || lines[1] != "第二行" {
		t.Fatalf("TextLines = %v", lines)
	}
	if !strings.HasSuffix(lines[2], "…") || len([]rune(lines[2])) != 21 {
		t.Errorf("超宽行应截断：%q", lines[2])
	}
	if got := TextLines("a\nb\nc", 2, 0); len(got) != 3 || !strings.Contains(got[2], "已截断") {
		t.Errorf("超行数应截断：%v", got)
	}
}
