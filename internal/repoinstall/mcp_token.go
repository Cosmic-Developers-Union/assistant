package repoinstall

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/instances"
)

// resolveMCPToken 只自动选择与目标站点绑定的凭据，按优先级：
//
//	GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >
//	credentials.json 里 (host, purpose=mcp) 的身份令牌 >
//	config.json 的 mcp_token（旧版字段，兼容读取）
//
// 显式覆盖出错时不降级，避免配置错误意外切换调用身份。
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
	credentialPath, err := mcpCredentialPath(configPath, getenv)
	if err != nil {
		return "", "", err
	}
	store, err := credentials.Load(credentialPath)
	if err != nil {
		return "", "", err
	}
	credential, ok, err := store.CredentialForIdentity(host, credentials.PurposeMCP)
	if err != nil {
		return "", "", err
	}
	if ok {
		return credential.Token, "assistant login (" + credentialPath + "，@" + credential.User + ")", nil
	}
	return legacyMCPToken(host, configPath, getenv)
}

// mcpCredentialPath 决定凭据库位置：ASSISTANT_CREDENTIALS > 与 config.json 同目录
// > 标准配置目录。
func mcpCredentialPath(configPath string, getenv func(string) string) (string, error) {
	if override := strings.TrimSpace(getenv("ASSISTANT_CREDENTIALS")); override != "" {
		return override, nil
	}
	effective := strings.TrimSpace(configPath)
	if effective == "" {
		effective = strings.TrimSpace(getenv("ASSISTANT_CONFIG"))
	}
	if effective == "" {
		path, err := instances.DefaultConfigPath()
		if err != nil {
			return "", err
		}
		effective = path
	}
	return credentials.PathFor(effective)
}

// legacyMCPToken 读取旧版写在 config.json 实例条目里的 mcp_token（迁移期兼容）。
func legacyMCPToken(host, configPath string, getenv func(string) string) (string, string, error) {
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
	if file == nil {
		return "", "", nil
	}
	for _, instance := range file.Instances {
		if !sameTeaHost(instance.Host, host) {
			continue
		}
		if token := strings.TrimSpace(instance.MCPToken); token != "" {
			return token, "config.json mcp_token (" + path + "，旧版字段)", nil
		}
		break
	}
	return "", "", nil
}
