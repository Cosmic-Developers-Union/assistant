package provider

import (
	"strings"
	"testing"

	"assistant/internal/claudecfg"
)

// 注册的 handler 在配置覆盖之上生效，并且不修改原始覆盖（会话间隔离）。
func TestApplyAppliesHandlerAndIsolatesBase(t *testing.T) {
	base := claudecfg.Overrides{Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example.com"}}
	overrides, err := Apply("OpenCode", "review", "session-1", base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	headers := overrides.Env[HeaderEnv]
	if !strings.Contains(headers, "x-opencode-session: session-1") ||
		!strings.Contains(headers, "x-session-affinity: session-1") {
		t.Errorf("%s = %q", HeaderEnv, headers)
	}
	if overrides.Env["ANTHROPIC_BASE_URL"] != "https://gateway.example.com" {
		t.Errorf("原覆盖丢失：%+v", overrides.Env)
	}
	if _, mutated := base.Env[HeaderEnv]; mutated {
		t.Errorf("原始覆盖被就地修改：%+v", base.Env)
	}
}

// 操作者显式配置的同名请求头优先；已有其他头原样保留；空会话 ID 自动生成。
func TestEnsureHeaderPreservesExisting(t *testing.T) {
	session := &Session{Env: map[string]string{
		HeaderEnv: "X-Other: keep\nx-opencode-session: operator-choice",
	}}
	session.EnsureHeader("X-Opencode-Session", "generated")
	headers := session.Env[HeaderEnv]
	if !strings.Contains(headers, "x-opencode-session: operator-choice") {
		t.Errorf("已配置头应保留：%q", headers)
	}
	if !strings.Contains(headers, "X-Other: keep") {
		t.Errorf("其他头应保留：%q", headers)
	}
	if strings.Count(strings.ToLower(headers), "x-opencode-session:") != 1 {
		t.Errorf("同名头不应重复：%q", headers)
	}
	empty := &Session{}
	empty.EnsureHeader("X-Test", "v")
	if empty.Env[HeaderEnv] != "X-Test: v" {
		t.Errorf("Env = %+v", empty.Env)
	}
	generated, err := Apply("unknown-provider", "chat", "", claudecfg.Overrides{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if generated.Env[HeaderEnv] != "" {
		t.Errorf("未注册 provider 不应注入头：%+v", generated.Env)
	}
}

// 别名匹配与空 provider 名原样通过。
func TestLookupAliases(t *testing.T) {
	for _, name := range []string{"opencode", "OpenCode", "zen", "opencode-zen", "opencode-go"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("Lookup(%q) 未命中", name)
		}
	}
	if _, ok := Lookup("anthropic"); ok {
		t.Error("未注册的 provider 不应命中")
	}
	base := claudecfg.Overrides{Env: map[string]string{"A": "b"}}
	untouched, err := Apply("", "triage", "", base)
	if err != nil || untouched.Env["A"] != "b" {
		t.Errorf("空 provider 应原样通过：%+v, %v", untouched, err)
	}
}

// 会话 ID 是 v4 UUID，且每次生成不同。
func TestNewSessionID(t *testing.T) {
	first, second := NewSessionID(), NewSessionID()
	if len(first) != 36 || first == second {
		t.Errorf("NewSessionID() = %q / %q", first, second)
	}
	if first[14] != '4' {
		t.Errorf("不是 v4 UUID：%q", first)
	}
}
