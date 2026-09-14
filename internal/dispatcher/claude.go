package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"assistant/internal/claudecfg"
)

// createSessionConfigDir 创建会话临时配置目录（settings 与合并后的 MCP 配置
// 同目录，容器形态只需挂载一个目录），返回路径与清理函数。
func createSessionConfigDir() (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", "assistant-claude-")
	if err != nil {
		return "", func() {}, fmt.Errorf("创建会话配置目录: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// writeClaudeSessionSettings 在会话配置目录生成 headless 会话的独立配置
// （--settings：环境变量 + 权限放行 + provider 覆盖）。
func writeClaudeSessionSettings(dir string, overrides claudecfg.Overrides) (string, error) {
	data, err := claudecfg.SessionSettingsWith(overrides)
	if err != nil {
		return "", fmt.Errorf("生成会话配置: %w", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("写入会话配置: %w", err)
	}
	return path, nil
}

// writeSessionMCPConfig 生成会话的 MCP 配置：provider 定义了原生 server 时，
// 把 basePath（宿主仓库 .mcp.json）与 provider server 合并写入会话配置目录
// （同名 server 由 provider 覆盖，provider env 缺省注入其 stdio server 的 env）。
// provider 未定义 mcp 或 basePath 为空时原样返回 basePath，不产生临时文件。
func writeSessionMCPConfig(dir, basePath string, overrides claudecfg.Overrides) (string, error) {
	if len(overrides.MCP) == 0 || basePath == "" {
		return basePath, nil
	}
	document := map[string]any{}
	data, err := os.ReadFile(basePath)
	switch {
	case err == nil:
		if trimmed := len(data); trimmed > 0 {
			if err := json.Unmarshal(data, &document); err != nil {
				return "", fmt.Errorf("解析 MCP 配置 %s: %w", basePath, err)
			}
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("读取 MCP 配置 %s: %w", basePath, err)
	}
	claudecfg.MergeMCPServers(document, overrides)
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("生成合并后的 MCP 配置: %w", err)
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("写入合并后的 MCP 配置: %w", err)
	}
	return path, nil
}

// sessionMountDirs 返回容器形态下需要挂载的宿主目录（MCP 配置与独立会话配置
// 所在目录，去重；容器内外路径一致）。
func sessionMountDirs(options SessionOptions) []string {
	seen := make(map[string]bool, 2)
	dirs := make([]string, 0, 2)
	for _, path := range []string{options.MCPConfigPath, options.SettingsPath} {
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}
