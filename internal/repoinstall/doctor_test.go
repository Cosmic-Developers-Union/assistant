package repoinstall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func statusOf(t *testing.T, findings []Finding, path string) string {
	t.Helper()
	for _, finding := range findings {
		if finding.Path == path {
			return finding.Status
		}
	}
	t.Fatalf("no finding for %s: %+v", path, findings)
	return ""
}

// doctor 对 install 产物做只读体检：安装前缺失，安装后全绿；文件被改写、
// 删除、替换为用户自有或遗留旧文件时给出对应状态。
func TestDoctorDetectsConfigurationDrift(t *testing.T) {
	options := testOptions(t)

	findings, err := Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	for _, path := range []string{ManagedSkillPath(), ManagedAgentPath(), ManagedClaudePath(), ManagedWorkflowPaths()[0], ".mcp.json", "opencode.json", options.CodexConfigPath} {
		if status := statusOf(t, findings, path); status != StatusMissing {
			t.Errorf("未安装时 %s 状态 = %s, want missing", path, status)
		}
	}

	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	findings, err = Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() after install error = %v", err)
	}
	for _, finding := range findings {
		if !finding.OK() {
			t.Errorf("install 后应全部正常: %+v", finding)
		}
	}

	// skill 被改写（保留 marker）→ outdated
	skillPath := filepath.Join(options.Dir, ManagedSkillPath())
	if err := os.WriteFile(skillPath, []byte("<!-- managed-by: assistant -->\n# hacked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// workflow 被删除 → missing
	workflowPath := filepath.Join(options.Dir, ManagedWorkflowPaths()[0])
	if err := os.Remove(workflowPath); err != nil {
		t.Fatal(err)
	}
	// AGENTS.md 被替换为用户自有（无 marker）→ unmanaged
	if err := os.WriteFile(filepath.Join(options.Dir, ManagedAgentPath()), []byte("# user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md 被替换为用户自有（无 @AGENTS.md）→ unmanaged
	if err := os.WriteFile(filepath.Join(options.Dir, ManagedClaudePath()), []byte("# user claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧版 automerge.yml（带 marker）→ legacy
	legacyPath := filepath.Join(options.Dir, LegacyWorkflowPaths()[0])
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("<!-- managed-by: assistant -->\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// opencode 放行被移除 → outdated
	if err := os.WriteFile(
		filepath.Join(options.Dir, "opencode.json"),
		[]byte(`{"mcp":{"gitea":{"type":"local","command":["assistant","mcp","gitea"],"enabled":true}}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// claude settings 被砍成只剩 MCP 放行（缺 env/只读权限）→ outdated
	if err := os.WriteFile(
		filepath.Join(options.Dir, ".claude", "settings.json"),
		[]byte(`{"enableAllProjectMcpServers":true,"permissions":{"allow":["mcp__gitea","mcp__gitea__*"]}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	findings, err = Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() after drift error = %v", err)
	}
	for path, want := range map[string]string{
		ManagedSkillPath():        StatusOutdated,
		ManagedWorkflowPaths()[0]: StatusMissing,
		ManagedAgentPath():        StatusUnmanaged,
		ManagedClaudePath():       StatusUnmanaged,
		LegacyWorkflowPaths()[0]:  StatusLegacy,
		"opencode.json":           StatusOutdated,
		".claude/settings.json":   StatusOutdated,
	} {
		if status := statusOf(t, findings, path); status != want {
			t.Errorf("%s 状态 = %s, want %s（%+v）", path, status, want, findings)
		}
	}
}
