package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"assistant/internal/claudecfg"
)

// writeClaudeSessionSettings 在临时目录生成 headless 会话的独立配置
// （--settings：环境变量 + 权限放行），返回路径与清理函数。
func writeClaudeSessionSettings() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "assistant-claude-")
	if err != nil {
		return "", func() {}, fmt.Errorf("创建会话配置目录: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	data, err := claudecfg.SessionSettings()
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("生成会话配置: %w", err)
	}
	path = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("写入会话配置: %w", err)
	}
	return path, cleanup, nil
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
