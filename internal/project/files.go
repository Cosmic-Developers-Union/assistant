package project

import (
	"bytes"
	_ "embed"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"uuid"
)

//go:embed assets/assistant.yml
var workflow []byte

// WorkflowOptions 的版本由调用方的构建信息装配，不从用户项目或站点推导。
type WorkflowOptions struct {
	Version        string
	Remove, DryRun bool
}

var artifactVersion = regexp.MustCompile(`^(?:v?[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*)?|sha-[a-f0-9]{40,64})$`)

// InstallWorkflow 只更新本工具标记的 workflow，不覆盖用户自有文件。
func InstallWorkflow(dir string, options WorkflowOptions) error {
	if !options.Remove && (len(options.Version) > 128 || !artifactVersion.MatchString(options.Version)) {
		return fmt.Errorf("构建产物缺少有效固定版本信息；请用 make build-local 重新构建，不接受 latest、dev 或分支名")
	}
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
	if options.DryRun {
		return nil
	}
	if options.Remove {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return root.Remove(name)
	}
	return replaceFile(root, name, bytes.ReplaceAll(workflow, []byte("__ASSISTANT_VERSION__"), []byte(options.Version)))
}

// MCPOptions 只允许显式强制安装覆盖冲突；卸载仍须验证归属。
// Log 报告备份位置，避免操作者丢失恢复原配置的线索。
type MCPOptions struct {
	Remove, DryRun, Force bool
	Log                   func(string)
}

// ConfigureMCP 安装或卸载一个命名 server，保留所有其他 server 和顶层字段。
// 工具配置只写包装层启动命令，不写 token；站点和凭据在启动时解析。
func ConfigureMCP(dir string, options MCPOptions) error {
	if options.Remove && options.Force {
		return fmt.Errorf("--force 仅用于 MCP 安装")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := root.ReadFile(".mcp.json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if options.Remove && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	config, parseErr := parseMCP(data)
	backup := parseErr != nil
	if parseErr != nil {
		if !options.Force {
			return fmt.Errorf("%w；修复 .mcp.json 或使用 install mcp --force 备份并重建", parseErr)
		}
		config = map[string]any{}
	}
	servers := map[string]any{}
	if raw, ok := config["mcpServers"]; ok {
		var valid bool
		servers, valid = raw.(map[string]any)
		if !valid {
			if !options.Force {
				return fmt.Errorf("mcpServers 必须是对象；使用 install mcp --force 备份并重建该字段")
			}
			backup = true
			servers = map[string]any{}
		}
	}
	const key = "gitea"
	if old, exists := servers[key]; exists {
		entry, ok := old.(map[string]any)
		if !ok || !ownedMCP(entry) {
			if !options.Force {
				return fmt.Errorf("gitea 已被非托管 server 使用，拒绝覆盖或删除；安装可用 --force 备份后覆盖")
			}
			backup = true
		}
	}
	// 兼容之前误用的名称；只迁移本工具生成的配置，保留用户自有 server。
	const legacyKey = "assistant-gitea"
	if entry, ok := servers[legacyKey].(map[string]any); ok && ownedMCP(entry) {
		delete(servers, legacyKey)
	}
	if options.Remove {
		delete(servers, key)
	} else {
		servers[key] = map[string]any{"command": "assistant", "args": []string{"mcp", "gitea"}}
	}
	config["mcpServers"] = servers
	if options.DryRun {
		return nil
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if backup {
		name := ".mcp.json.assistant-backup-" + uuid.New().String()
		if err := root.WriteFile(name, data, 0600); err != nil {
			return fmt.Errorf("备份原 MCP 配置: %w", err)
		}
		if options.Log != nil {
			options.Log("原 MCP 配置已备份：" + filepath.Join(dir, name))
		}
	}
	return replaceFile(root, ".mcp.json", append(encoded, '\n'))
}

func parseMCP(data []byte) (map[string]any, error) {
	config := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return config, nil
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析已有 MCP 配置: %w", err)
	}
	if config == nil {
		return nil, fmt.Errorf("MCP 配置必须是对象")
	}
	return config, nil
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
