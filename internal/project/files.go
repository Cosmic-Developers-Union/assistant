package project

import (
	"bytes"
	_ "embed"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"
)

//go:embed assets/assistant.yml
var workflow []byte

// InstallWorkflow 只更新本工具标记的 workflow，不覆盖用户自有文件。
func InstallWorkflow(dir string, remove, dry bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Join(".gitea", "workflows", "assistant.yml")
	data, err := root.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !bytes.HasPrefix(data, []byte("# managed-by: assistant\n")) {
		return fmt.Errorf("workflow 未由 assistant 托管，拒绝覆盖或删除")
	}
	if dry {
		return nil
	}
	if remove {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return root.Remove(name)
	}
	return replaceFile(root, name, workflow)
}

// ConfigureMCP 安装或卸载一个命名 server，保留所有其他 server 和顶层字段。
// 工具配置只写包装层启动命令，不写 token；站点和凭据在启动时解析。
func ConfigureMCP(dir string, remove, dry bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := root.ReadFile(".mcp.json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if remove && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	config := map[string]any{}
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("解析已有 MCP 配置: %w", err)
		}
	}
	if config == nil {
		return fmt.Errorf("MCP 配置必须是对象")
	}
	servers := map[string]any{}
	if raw, ok := config["mcpServers"]; ok {
		var valid bool
		servers, valid = raw.(map[string]any)
		if !valid {
			return fmt.Errorf("mcpServers 必须是对象")
		}
	}
	const key = "gitea"
	if old, exists := servers[key]; exists {
		entry, ok := old.(map[string]any)
		if !ok || !ownedMCP(entry) {
			return fmt.Errorf("gitea 已被非托管 server 使用，拒绝覆盖或删除")
		}
	}
	// 兼容之前误用的名称；只迁移本工具生成的配置，保留用户自有 server。
	const legacyKey = "assistant-gitea"
	if entry, ok := servers[legacyKey].(map[string]any); ok && ownedMCP(entry) {
		delete(servers, legacyKey)
	}
	if remove {
		delete(servers, key)
	} else {
		servers[key] = map[string]any{"command": "assistant", "args": []string{"mcp", "gitea"}}
	}
	config["mcpServers"] = servers
	if dry {
		return nil
	}
	data, err = json.Marshal(config)
	if err != nil {
		return err
	}
	return replaceFile(root, ".mcp.json", append(data, '\n'))
}
func ownedMCP(entry map[string]any) bool {
	if len(entry) != 2 || entry["command"] != "assistant" {
		return false
	}
	args, ok := entry["args"].([]any)
	if !ok || len(args) < 2 || args[0] != "mcp" || args[1] != "gitea" {
		return false
	}
	return len(args) == 2 || len(args) == 4 && args[2] == "--instance"
}
func replaceFile(root *os.Root, name string, data []byte) error {
	if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".assistant-write-"+uuid.New().String())
	if err := root.WriteFile(temp, data, 0644); err != nil {
		return err
	}
	defer root.Remove(temp)
	return root.Rename(temp, name)
}
