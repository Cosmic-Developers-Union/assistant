package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"assistant/internal/claudecfg"
)

// writeSessionMCPConfig：provider 定义原生 MCP 时与仓库 .mcp.json 合并注入，
// 未定义时原样使用仓库配置。
func TestWriteSessionMCPConfig(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(base, []byte(`{"mcpServers": {"gitea": {"command": "assistant", "args": ["mcp", "gitea"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 无 provider mcp：原样返回仓库配置
	unchanged, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{Env: map[string]string{"A": "b"}})
	if err != nil || unchanged != base {
		t.Fatalf("unchanged = %q, err = %v", unchanged, err)
	}

	// provider mcp：合并进临时配置，同名覆盖，stdio server 继承 provider env
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
	if merged == base {
		t.Fatal("provider mcp 应产生临时合并配置")
	}
	data, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	servers, _ := document["mcpServers"].(map[string]any)
	if gitea, _ := servers["gitea"].(map[string]any); gitea["command"] != "provider-gitea" {
		t.Errorf("同名 server 应被覆盖：%+v", servers)
	}
	search, _ := servers["search"].(map[string]any)
	searchEnv, _ := search["env"].(map[string]any)
	if searchEnv["GATEWAY_KEY"] != "secret" {
		t.Errorf("provider env 未注入 server：%+v", searchEnv)
	}

	// 仓库缺少 .mcp.json（未就绪/对话场景）时仍生成 provider 配置
	missing, err := writeSessionMCPConfig(dir, filepath.Join(dir, "absent.json"), claudecfg.Overrides{
		MCP: map[string]any{"search": map[string]any{"command": "search-mcp"}},
	})
	if err != nil {
		t.Fatalf("缺少 base 时应可用 provider 配置: %v", err)
	}
	if _, err := os.Stat(missing); err != nil {
		t.Errorf("合并配置未写入：%v", err)
	}

	// base 非法 JSON 直接报错，不静默丢弃仓库工具面
	if err := os.WriteFile(base, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{
		MCP: map[string]any{"search": map[string]any{"command": "search-mcp"}},
	}); err == nil {
		t.Error("非法 base 应报错")
	}
}
