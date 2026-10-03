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
	if err := InstallWorkflow(dir, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("演练写了文件")
	}
	for range 2 {
		if err := InstallWorkflow(dir, false, false); err != nil {
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
		if err := InstallWorkflow(dir, remove, false); err == nil {
			t.Fatal("未拒绝非托管 workflow")
		}
	}
	if err := os.WriteFile(path, workflow, 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := InstallWorkflow(dir, true, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := InstallWorkflow("/missing/project-dir", false, false); err == nil {
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
	if err := ConfigureMCP(dir, "work-ai", false, true); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != original {
		t.Fatal("演练修改已有配置")
	}
	for range 2 {
		if err := ConfigureMCP(dir, "work-ai", false, false); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["note"] != "保留" || !strings.Contains(string(data), "other-tool") || !strings.Contains(string(data), "--instance") {
		t.Fatal(string(data))
	}
	for range 2 {
		if err := ConfigureMCP(dir, "", true, false); err != nil {
			t.Fatal(err)
		}
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), `"gitea"`) || !strings.Contains(string(data), "existing-secret") {
		t.Fatal(string(data))
	}
	if err := ConfigureMCP(t.TempDir(), "", true, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"null", "bad", `{"mcpServers":[]}`, `{"mcpServers":{"gitea":{"command":"user-command"}}}`} {
		if err := os.WriteFile(path, []byte(bad), 0644); err != nil {
			t.Fatal(err)
		}
		if err := ConfigureMCP(dir, "work-ai", false, false); err == nil {
			t.Fatal("无效/非托管配置被覆盖", bad)
		}
	}
	if err := ConfigureMCP("/missing/project-dir", "name", false, false); err == nil {
		t.Fatal("缺失目录被接受")
	}
}
func TestMCPDefaultAndBoundConfigurationsCanReplaceEachOther(t *testing.T) {
	dir := t.TempDir()
	for _, instance := range []string{"", "work-ai", "", "work-merge"} {
		if err := ConfigureMCP(dir, instance, false, false); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Servers map[string]struct {
				Args []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		args := config.Servers["gitea"].Args
		if instance == "" {
			if len(args) != 2 || args[0] != "mcp" || args[1] != "gitea" {
				t.Fatal(args)
			}
		} else if len(args) != 4 || args[2] != "--instance" || args[3] != instance {
			t.Fatal(args)
		}
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
			err := ConfigureMCP(dir, "", scenario.remove, scenario.dry)
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
		})
	}
}

func TestManagedFilesRejectSymlinkEscapeAndFilesystemFailures(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".gitea")); err != nil {
		t.Skip(err)
	}
	if err := InstallWorkflow(dir, false, false); err == nil {
		t.Fatal("workflow 越界 symlink 被接受")
	}
	if err := os.Symlink(filepath.Join(outside, "mcp.json"), filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureMCP(dir, "name", false, false); err == nil {
		t.Fatal("MCP 越界 symlink 被接受")
	}
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".mcp.json"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureMCP(dir, "name", false, false); err == nil {
		t.Fatal("目录被当作配置")
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitea"), []byte("阻挡目录"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := InstallWorkflow(dir, false, false); err == nil {
		t.Fatal("目录创建错误被忽略")
	}
}
