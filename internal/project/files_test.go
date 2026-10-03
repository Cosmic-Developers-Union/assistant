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
	if strings.Contains(string(data), "assistant-gitea") || !strings.Contains(string(data), "existing-secret") {
		t.Fatal(string(data))
	}
	if err := ConfigureMCP(t.TempDir(), "", true, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"null", "bad", `{"mcpServers":[]}`, `{"mcpServers":{"assistant-gitea":{"command":"user-command"}}}`} {
		if err := os.WriteFile(path, []byte(bad), 0644); err != nil {
			t.Fatal(err)
		}
		if err := ConfigureMCP(dir, "work-ai", false, false); err == nil {
			t.Fatal("无效/非托管配置被覆盖", bad)
		}
	}
	if err := ConfigureMCP(dir, "", false, false); err == nil {
		t.Fatal("空实例被接受")
	}
	if err := ConfigureMCP("/missing/project-dir", "name", false, false); err == nil {
		t.Fatal("缺失目录被接受")
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
