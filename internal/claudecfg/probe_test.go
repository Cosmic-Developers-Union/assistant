package claudecfg

import (
	"os"
	"path/filepath"
	"testing"
)

// CredentialSource：provider env > settings apiKeyHelper > 进程环境；都没有时为空，
// 供启动自检告警（会话不读 ~/.claude 登录态，没有凭据就一定认证失败）。
func TestCredentialSource(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	overrides := Overrides{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}}
	if got := CredentialSource(overrides); got != "provider env ANTHROPIC_AUTH_TOKEN" {
		t.Errorf("CredentialSource = %q", got)
	}
	overrides = Overrides{Settings: map[string]any{"apiKeyHelper": "echo key"}}
	if got := CredentialSource(overrides); got != "settings apiKeyHelper" {
		t.Errorf("CredentialSource = %q", got)
	}
	if got := CredentialSource(Overrides{Settings: map[string]any{"apiKeyHelper": "  "}}); got != "" {
		t.Errorf("空 apiKeyHelper 不该算凭据：%q", got)
	}
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	if got := CredentialSource(Overrides{}); got != "进程环境 ANTHROPIC_API_KEY" {
		t.Errorf("CredentialSource = %q", got)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if got := CredentialSource(Overrides{Env: map[string]string{"OTHER": "x"}}); got != "" {
		t.Errorf("不该把无关 env 当凭据：%q", got)
	}
}

// claude 能力探测：版本行与 --bare 支持度都按可执行文件读取，探测不到时返回
// 保守结果（空版本 / 不支持）。
func TestClaudeVersionAndBareProbe(t *testing.T) {
	dir := t.TempDir()
	supported := filepath.Join(dir, "claude-bare")
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo '2.1.270 (Claude Code)';;\n  --help) echo '  --bare   Minimal mode: skip hooks';;\nesac\n"
	if err := os.WriteFile(supported, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeVersion(supported); got != "2.1.270 (Claude Code)" {
		t.Errorf("ClaudeVersion = %q", got)
	}
	if !SupportsBare(supported) {
		t.Error("应识别出 --bare")
	}

	legacy := filepath.Join(dir, "claude-legacy")
	if err := os.WriteFile(legacy, []byte("#!/bin/sh\ncase \"$1\" in\n  --version) echo '1.0.0';;\n  --help) echo '  --verbose  Verbose';;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if SupportsBare(legacy) {
		t.Error("不支持 --bare 的实现不该被判为支持")
	}
	if SupportsBare(filepath.Join(dir, "absent-claude")) {
		t.Error("探测不到可执行文件时应返回 false")
	}
	if got := ClaudeVersion(filepath.Join(dir, "absent-claude")); got != "" {
		t.Errorf("ClaudeVersion = %q", got)
	}
}
