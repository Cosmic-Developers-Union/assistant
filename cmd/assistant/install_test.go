package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/repoinstall"
)

// TestInstallGiteaActionsWritesWorkflow 确认 --dir / --image 透传到
// InstallWorkflows，并且只在非 dry-run 时写盘、打印就绪。
func TestInstallGiteaActionsWritesWorkflow(t *testing.T) {
	target := t.TempDir()
	var stdout bytes.Buffer
	command := newInstallCommand()
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{
		"gitea-actions",
		"--dir", target,
		"--image", "example.com/assistant:test",
	})

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}

	path := filepath.Join(target, filepath.FromSlash(repoinstall.ManagedWorkflowPaths()[0]))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 workflow: %v", err)
	}
	if !strings.Contains(string(content), "example.com/assistant:test") {
		t.Errorf("workflow 未使用 --image: %q", content)
	}
	if !strings.Contains(string(content), repoinstall.Marker) {
		t.Errorf("workflow 缺少 marker 标记: %q", content)
	}
	if !strings.Contains(stdout.String(), "就绪") {
		t.Errorf("缺少就绪提示: %q", stdout.String())
	}
}

// TestInstallGiteaActionsDryRunWritesNothing 确认 dry-run 既不写盘也不打印就绪。
func TestInstallGiteaActionsDryRunWritesNothing(t *testing.T) {
	target := t.TempDir()
	var stdout bytes.Buffer
	command := newInstallCommand()
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"gitea-actions", "--dir", target, "--dry-run"})

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(target, ".gitea")); !os.IsNotExist(err) {
		t.Errorf("dry-run 不应写盘: err = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "dry-run：写入") {
		t.Errorf("dry-run 应说明将要写入的内容: %q", output)
	}
	if strings.Contains(output, "就绪") {
		t.Errorf("dry-run 不应打印就绪: %q", output)
	}
}

// TestInstallGiteaActionsClearsLegacyWorkflow 确认 InstallWorkflows 顺带清理带
// marker 的旧版独立 workflow（迁移路径），且放过非 assistant 生成的文件。
func TestInstallGiteaActionsClearsLegacyWorkflow(t *testing.T) {
	target := t.TempDir()
	legacyDir := filepath.Join(target, ".gitea", "workflows")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	managedLegacy := filepath.Join(legacyDir, "automerge.yml")
	if err := os.WriteFile(
		managedLegacy,
		[]byte("# "+repoinstall.Marker+"\nname: legacy\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	handWritten := filepath.Join(legacyDir, "custom.yml")
	if err := os.WriteFile(handWritten, []byte("name: 手写\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	command := newInstallCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"gitea-actions", "--dir", target})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}

	if _, err := os.Stat(managedLegacy); !os.IsNotExist(err) {
		t.Errorf("带 marker 的旧版 workflow 应被清理: err = %v", err)
	}
	if _, err := os.Stat(handWritten); err != nil {
		t.Errorf("手写 workflow 不应被触碰: err = %v", err)
	}
}
