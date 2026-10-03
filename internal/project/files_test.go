package project

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedWorkflowLifecycleAndPreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitea", "workflows", "assistant.yml")
	if err := InstallWorkflow(dir, WorkflowOptions{Version: "v1.2.3", Remove: false, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("演练写了文件")
	}
	for range 2 {
		if err := InstallWorkflow(dir, WorkflowOptions{Version: "v1.2.3", Remove: false, DryRun: false}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "GITEA_REPOSITORY") || strings.Contains(string(data), "private-token") {
		t.Fatal(string(data))
	}
	if err := os.WriteFile(path, []byte("name: 用户 workflow"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{true, false} {
		if err := InstallWorkflow(dir, WorkflowOptions{Version: "v1.2.3", Remove: remove, DryRun: false}); err == nil {
			t.Fatal("未拒绝非托管 workflow")
		}
	}
	if err := os.WriteFile(path, workflow, 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := InstallWorkflow(dir, WorkflowOptions{Version: "v1.2.3", Remove: true, DryRun: false}); err != nil {
			t.Fatal(err)
		}
	}
	if err := InstallWorkflow("/missing/project-dir", WorkflowOptions{Version: "v1.2.3", Remove: false, DryRun: false}); err == nil {
		t.Fatal("缺失目录被接受")
	}
}
func TestMCPPreservesOtherServersAndNeverStoresSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	original := `{"note":"保留", "mcpServers":{"other":{"command":"other-tool","env":{"KEY":"existing-secret"}}}}`
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureMCP(dir, MCPOptions{Remove: false, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != original {
		t.Fatal("演练修改已有配置")
	}
	for range 2 {
		if err := ConfigureMCP(dir, MCPOptions{Remove: false, DryRun: false}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["note"] != "保留" || !strings.Contains(string(data), "other-tool") || strings.Contains(string(data), "--instance") {
		t.Fatal(string(data))
	}
	for range 2 {
		if err := ConfigureMCP(dir, MCPOptions{Remove: true, DryRun: false}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), `"gitea"`) || !strings.Contains(string(data), "existing-secret") {
		t.Fatal(string(data))
	}
	if err := ConfigureMCP(t.TempDir(), MCPOptions{Remove: true, DryRun: false}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"null", "bad", `{"mcpServers":[]}`, `{"mcpServers":{"gitea":{"command":"user-command"}}}`} {
		if err := os.WriteFile(path, []byte(bad), 0644); err != nil {
			t.Fatal(err)
		}
		if err := ConfigureMCP(dir, MCPOptions{Remove: false, DryRun: false}); err == nil {
			t.Fatal("无效/非托管配置被覆盖", bad)
		}
	}
	if err := ConfigureMCP("/missing/project-dir", MCPOptions{Remove: false, DryRun: false}); err == nil {
		t.Fatal("缺失目录被接受")
	}
}
func TestMCPNameMigrationAndCollision(t *testing.T) {
	for _, scenario := range []struct {
		name, original string
		remove, dry    bool
		wantError      bool
		keepLegacy     bool
	}{
		{"默认旧配置", `{"assistant-gitea":{"command":"assistant","args":["mcp","gitea"]}}`, false, false, false, false},
		{"绑定旧配置", `{"assistant-gitea":{"command":"assistant","args":["mcp","gitea","--instance","work-ai"]}}`, false, false, false, false},
		{"移除新增绑定参数", `{"gitea":{"command":"assistant","args":["mcp","gitea","--instance","work-ai"]}}`, false, false, false, false},
		{"卸载旧配置", `{"assistant-gitea":{"command":"assistant","args":["mcp","gitea"]}}`, true, false, false, false},
		{"演练迁移", `{"assistant-gitea":{"command":"assistant","args":["mcp","gitea"]}}`, false, true, false, true},
		{"保留旧名用户配置", `{"assistant-gitea":{"command":"user-tool"}}`, false, false, false, true},
		{"保留旧名用户配置卸载", `{"assistant-gitea":{"command":"user-tool"}}`, true, false, false, true},
		{"新名冲突", `{"gitea":{"command":"user-tool"},"assistant-gitea":{"command":"assistant","args":["mcp","gitea"]}}`, false, false, true, true},
		{"卸载新名冲突", `{"gitea":{"command":"user-tool"}}`, true, false, true, false},
		{"清理重复托管配置", `{"gitea":{"command":"assistant","args":["mcp","gitea"]},"assistant-gitea":{"command":"assistant","args":["mcp","gitea"]}}`, false, false, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".mcp.json")
			original := `{"note":"保留","mcpServers":` + scenario.original + `}`
			if err := os.WriteFile(path, []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			err := ConfigureMCP(dir, MCPOptions{Remove: scenario.remove, DryRun: scenario.dry})
			if (err != nil) != scenario.wantError {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.dry || scenario.wantError {
				if string(data) != original {
					t.Fatal("演练或冲突修改了配置", string(data))
				}
				return
			}
			var config struct {
				Note    string                    `json:"note"`
				Servers map[string]map[string]any `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			_, legacy := config.Servers["assistant-gitea"]
			_, current := config.Servers["gitea"]
			if config.Note != "保留" || legacy != scenario.keepLegacy || current == scenario.remove {
				t.Fatal(string(data))
			}
			if scenario.keepLegacy && config.Servers["assistant-gitea"]["command"] != "user-tool" {
				t.Fatal("用户配置被修改", string(data))
			}
			if current && strings.Contains(string(data), "--instance") {
				t.Fatal("迁移后仍生成额外 MCP 参数")
			}
		})
	}
}

func TestManagedFilesRejectSymlinkEscapeAndFilesystemFailures(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".gitea")); err != nil {
		t.Skip(err)
	}
	if err := InstallWorkflow(dir, WorkflowOptions{Version: "v1.2.3", Remove: false, DryRun: false}); err == nil {
		t.Fatal("workflow 越界 symlink 被接受")
	}
	if err := os.Symlink(filepath.Join(outside, "mcp.json"), filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureMCP(dir, MCPOptions{Remove: false, DryRun: false}); err == nil {
		t.Fatal("MCP 越界 symlink 被接受")
	}
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".mcp.json"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureMCP(dir, MCPOptions{Remove: false, DryRun: false}); err == nil {
		t.Fatal("目录被当作配置")
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitea"), []byte("阻挡目录"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := InstallWorkflow(dir, WorkflowOptions{Version: "v1.2.3", Remove: false, DryRun: false}); err == nil {
		t.Fatal("目录创建错误被忽略")
	}
}

func TestMCPEmptyFiles(t *testing.T) {
	for _, original := range []string{"", " \n\t", "{}"} {
		t.Run(original, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".mcp.json")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ConfigureMCP(dir, MCPOptions{}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), `"gitea"`) {
				t.Fatal(string(data), err)
			}
			backups, _ := filepath.Glob(path + ".assistant-backup-*")
			if len(backups) != 0 {
				t.Fatal("空配置产生多余备份")
			}
		})
	}
}

func TestMCPForceBacksUpAndPreservesRecoverableFields(t *testing.T) {
	for _, original := range []string{
		`{"broken":`, "null", "[]",
		`{"note":"保留","mcpServers":[]}`,
		`{"note":"保留","mcpServers":{"gitea":{"command":"user-tool"},"other":{"command":"other-tool"}}}`,
	} {
		t.Run(original, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".mcp.json")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ConfigureMCP(dir, MCPOptions{}); err == nil || !strings.Contains(err.Error(), "--force") {
				t.Fatal(err)
			}
			if err := ConfigureMCP(dir, MCPOptions{Force: true, DryRun: true}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			backups, _ := filepath.Glob(path + ".assistant-backup-*")
			if string(before) != original || len(backups) != 0 {
				t.Fatal("失败或演练写入配置")
			}
			var message string
			if err := ConfigureMCP(dir, MCPOptions{Force: true, Log: func(s string) { message = s }}); err != nil {
				t.Fatal(err)
			}
			backups, _ = filepath.Glob(path + ".assistant-backup-*")
			if len(backups) != 1 || !strings.Contains(message, backups[0]) {
				t.Fatal(backups, message)
			}
			backup, err := os.ReadFile(backups[0])
			if err != nil || string(backup) != original {
				t.Fatal("备份不完整", err)
			}
			info, err := os.Stat(backups[0])
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("备份权限", err)
			}
			data, _ := os.ReadFile(path)
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(original, `"note"`) && config["note"] != "保留" {
				t.Fatal("有效顶层字段被丢弃")
			}
			if strings.Contains(original, "other-tool") && !strings.Contains(string(data), "other-tool") {
				t.Fatal("其他 server 被丢弃")
			}
			if !strings.Contains(string(data), `"command":"assistant"`) || strings.Contains(string(data), "--instance") {
				t.Fatal(string(data))
			}
			if err := ConfigureMCP(dir, MCPOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
			after, _ := filepath.Glob(path + ".assistant-backup-*")
			if len(after) != 1 {
				t.Fatal("幂等安装产生额外备份")
			}
		})
	}
	if err := ConfigureMCP(t.TempDir(), MCPOptions{Remove: true, Force: true}); err == nil {
		t.Fatal("强制卸载被接受")
	}
}

func TestMCPForceDoesNotBypassFilesystemBoundaries(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(outside, "secret")
	if err := os.WriteFile(path, []byte("原配置"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, ".mcp.json")); err != nil {
		t.Skip(err)
	}
	if err := ConfigureMCP(dir, MCPOptions{Force: true}); err == nil {
		t.Fatal("强制安装绕过越界限制")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "原配置" {
		t.Fatal("修改了外部配置")
	}
}

func TestWorkflowVersionIsFixedAndCanBeUpdated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitea", "workflows", "assistant.yml")
	for _, v := range []string{"", "latest", "dev", "main", "v1", "v1.2.3\nmalicious: value", "v1.2.3/foo", "v1.2.3:latest", "v1.2.3-rc..1", "v1.2.3-" + strings.Repeat("a", 128)} {
		if err := InstallWorkflow(dir, WorkflowOptions{Version: v}); err == nil {
			t.Fatal("接受非固定版本", v)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("无效版本写文件")
		}
	}
	for _, v := range []string{"v1.2.3", "v2.3.4-rc.1", "v2.3.4-rc-1", "sha-" + strings.Repeat("a", 40)} {
		if err := InstallWorkflow(dir, WorkflowOptions{Version: v}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || strings.Count(string(data), "ghcr.io/cosmic-developers-union/assistant:"+v) != 2 || strings.Contains(string(data), "assistant:latest") || strings.Contains(string(data), "__ASSISTANT_VERSION__") {
			t.Fatal(string(data), err)
		}
	}
	if err := InstallWorkflow(dir, WorkflowOptions{Remove: true}); err != nil {
		t.Fatal("卸载要求版本", err)
	}
}

func TestMCPBackupFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	original := []byte("broken")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	// root 或不支持权限位的文件系统无法模拟写入被拒绝。
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, nil, 0600); err == nil {
		_ = os.Remove(probe)
		t.Skip("当前用户可绕过目录权限")
	}
	if err := ConfigureMCP(dir, MCPOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "备份") {
		t.Fatal("备份失败后继续覆盖", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) {
		t.Fatal("备份失败破坏原文件", err)
	}
}
