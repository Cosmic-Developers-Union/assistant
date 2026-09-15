package repoinstall

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"assistant/internal/instances"
)

// resolveMCPToken 仅自动选择与目标站点绑定的凭据。显式覆盖出错时不降级，
// 避免配置错误意外切换调用身份。
func resolveMCPToken(host, configPath string, getenv func(string) string) (string, string, error) {
	if token := strings.TrimSpace(getenv("GITEA_ACCESS_TOKEN")); token != "" {
		return token, "GITEA_ACCESS_TOKEN", nil
	}
	if path := strings.TrimSpace(getenv("GITEA_ACCESS_TOKEN_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("读取 GITEA_ACCESS_TOKEN_FILE: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", "", fmt.Errorf("GITEA_ACCESS_TOKEN_FILE 指定的文件 %s 为空", path)
		}
		return token, path, nil
	}
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = strings.TrimSpace(getenv("ASSISTANT_CONFIG"))
	}
	explicit := path != ""
	if path == "" {
		var err error
		path, err = instances.DefaultConfigPath()
		if err != nil {
			return "", "", err
		}
	}
	file, err := instances.Load(path)
	if err != nil && (explicit || !errors.Is(err, os.ErrNotExist)) {
		return "", "", err
	}
	if file != nil {
		for _, instance := range file.Instances {
			if !sameTeaHost(instance.Host, host) {
				continue
			}
			source := "assistant login --mcp (" + path + ")"
			if token := strings.TrimSpace(instance.MCPToken); token != "" {
				return token, source, nil
			}
			break
		}
	}
	return "", "", nil
}
