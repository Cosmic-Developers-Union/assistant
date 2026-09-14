package provider

import (
	"strings"
	"testing"

	"assistant/internal/claudecfg"
)

// 令牌简写按预设落到正确的环境变量，预设默认（端点/超时/模型映射）自动打底。
func TestResolvePresetAndTokenShorthand(t *testing.T) {
	tests := []struct {
		name     string
		tokenEnv string
		baseURL  string
	}{
		{"zhipu", "ANTHROPIC_AUTH_TOKEN", "https://api.z.ai/api/anthropic"},
		{"glm", "ANTHROPIC_AUTH_TOKEN", "https://api.z.ai/api/anthropic"},
		{"kimi", "ANTHROPIC_API_KEY", "https://api.kimi.com/coding/"},
		{"minimax", "ANTHROPIC_AUTH_TOKEN", "https://api.minimax.io/anthropic"},
		{"opencode", "ANTHROPIC_AUTH_TOKEN", "https://opencode.ai/zen"},
		{"zen", "ANTHROPIC_AUTH_TOKEN", "https://opencode.ai/zen"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overrides, err := Resolve(test.name, claudecfg.Overrides{}, claudecfg.Overrides{}, "secret-token")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if overrides.Env[test.tokenEnv] != "secret-token" {
				t.Errorf("%s = %q, want token", test.tokenEnv, overrides.Env[test.tokenEnv])
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
