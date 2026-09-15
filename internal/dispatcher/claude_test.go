package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"assistant/internal/claudecfg"
)

// writeSessionMCPConfig：assistant 指定的 gitea server 恒注入（覆盖仓库里的同名
// 条目），仓库其它 server 保留，provider 原生 MCP 最后合并；仓库没有 .mcp.json
// 也照常生成——评审不依赖仓库内容。
func TestWriteSessionMCPConfig(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "repo.json")
	if err := os.WriteFile(base, []byte(`{"mcpServers": {
		"gitea": {"command": "stale-assistant", "args": ["mcp", "gitea"]},
		"project": {"command": "project-mcp"}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 无 provider mcp：也生成会话配置，gitea 条目由 assistant 注入
	generated, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{Env: map[string]string{"A": "b"}})
	if err != nil {
		t.Fatalf("writeSessionMCPConfig: %v", err)
	}
	if generated == base {
		t.Fatal("会话 MCP 配置必须是新生成的合并文件，不能复用仓库文件")
	}
	servers := readMCPServers(t, generated)
	gitea, _ := servers[claudecfg.MCPServerGitea].(map[string]any)
	if gitea["command"] != claudecfg.AssistantCommand() {
		t.Errorf("gitea server 应由 assistant 指定：%+v", gitea)
	}
	if args, _ := gitea["args"].([]any); len(args) != 2 || args[0] != "mcp" || args[1] != "gitea" {
		t.Errorf("gitea server args 不对：%+v", gitea)
	}
	if _, ok := servers["project"]; !ok {
		t.Errorf("仓库其它 server 应保留：%+v", servers)
	}

	// provider mcp：合并进同一文件，同名覆盖，stdio server 继承 provider env
	merged, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{
		Env: map[string]string{"GATEWAY_KEY": "secret"},
		MCP: map[string]any{
			"search": map[string]any{"command": "search-mcp"},
			"gitea":  map[string]any{"command": "provider-gitea"},
		},
	})
	if err != nil {
		t.Fatalf("writeSessionMCPConfig: %v", err)
	}
	servers = readMCPServers(t, merged)
	if gitea, _ := servers["gitea"].(map[string]any); gitea["command"] != "provider-gitea" {
		t.Errorf("provider 同名 server 应覆盖 assistant 默认：%+v", servers)
	}
	search, _ := servers["search"].(map[string]any)
	searchEnv, _ := search["env"].(map[string]any)
	if searchEnv["GATEWAY_KEY"] != "secret" {
		t.Errorf("provider env 未注入 server：%+v", searchEnv)
	}

	// 仓库没有 .mcp.json：gitea 条目照样在，评审不因仓库缺文件而失去工具面
	missing, err := writeSessionMCPConfig(dir, filepath.Join(dir, "absent.json"), claudecfg.Overrides{})
	if err != nil {
		t.Fatalf("缺少 base 时应可用 assistant 注入的 MCP: %v", err)
	}
	if _, ok := readMCPServers(t, missing)[claudecfg.MCPServerGitea]; !ok {
		t.Error("仓库缺 .mcp.json 时 gitea server 仍应注入")
	}

	// base 非法 JSON 直接报错，不静默丢弃仓库工具面
	if err := os.WriteFile(base, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{}); err == nil {
		t.Error("非法 base 应报错")
	}
}

func readMCPServers(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	servers, _ := document["mcpServers"].(map[string]any)
	if servers == nil {
		t.Fatalf("MCP 配置缺少 mcpServers：%s", data)
	}
	return servers
}
