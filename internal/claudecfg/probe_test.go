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

// 探测缓存与空白路径：同一个可执行文件第二次探测走缓存（不再执行），空白路径
// 视为未配置直接返回保守值。
func TestProbeCachesAndBlankPath(t *testing.T) {
	if got := ClaudeVersion("   "); got != "" {
		t.Errorf("空白路径不该触发探测：%q", got)
	}
	if SupportsBare("\t\n") {
		t.Error("空白路径不该被当作支持 --bare")
	}

	dir := t.TempDir()
	// 首次探测后把脚本换成会失败的内容：若仍走缓存，结果应保持不变
	bin := filepath.Join(dir, "claude-cached")
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo '9.9.9';;\n  --help) echo '  --bare';;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeVersion(bin); got != "9.9.9" {
		t.Fatalf("首次 ClaudeVersion = %q", got)
	}
	if !SupportsBare(bin) {
		t.Fatal("首次 SupportsBare 应为 true")
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeVersion(bin); got != "9.9.9" {
		t.Errorf("第二次应读缓存：%q", got)
	}
	if !SupportsBare(bin) {
		t.Error("第二次 SupportsBare 应读缓存")
	}

	// 版本行带换行时只取第一行（多行输出折成一行）
	multi := filepath.Join(dir, "claude-multiline")
	script = "#!/bin/sh\ncase \"$1\" in\n  --version) printf '3.1.4 (Claude Code)\\nextra line\\n';;\nesac\n"
	if err := os.WriteFile(multi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeVersion(multi); got != "3.1.4 (Claude Code)" {
		t.Errorf("多行版本应只取第一行：%q", got)
	}
}
