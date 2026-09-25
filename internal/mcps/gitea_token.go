package mcps

import (
	"fmt"
	"os"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// resolveGiteaToken 选择与目标站点绑定的 MCP 令牌，按优先级：
//
//	GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >
//	credentials.json 里 (host, purpose=mcp) 的登录令牌
//
// 环境变量优先是为了让评审会话的显式注入（dispatcher 以 reviewer 身份启动会话）
// 能覆盖本地登录；其余情况一律以凭据库为准。显式覆盖出错时不降级，避免配置错误
// 意外切换调用身份。
func resolveGiteaToken(host string, getenv func(string) string) (string, string, error) {
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
	credentialPath, err := credentials.Path()
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
	if !ok {
		return "", "", nil
	}
	return credential.Token, "assistant login add (" + credentialPath + "，@" + credential.User + ")", nil
}
