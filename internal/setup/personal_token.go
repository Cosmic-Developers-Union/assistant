package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CreateMCPToken 创建当前个人账号的专用长期令牌，不操作该账号的其他令牌。
// 密码仅用于本次 Basic Auth，TOTP 通过 Gitea 专用请求头传递。
func CreateMCPToken(ctx context.Context, host, user, password, totp, name string) (string, error) {
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("无效的 Gitea 站点地址")
	}
	if user == "" || password == "" || name == "" {
		return "", fmt.Errorf("创建 MCP 令牌需要用户名、密码和令牌名")
	}
	body, err := json.Marshal(map[string]any{
		"name":   name,
		"scopes": []string{"write:repository", "write:issue", "write:organization", "write:package", "read:user"},
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(host, "/")+"/api/v1/users/"+url.PathEscape(user)+"/tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(user, password)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if totp != "" {
		request.Header.Set("X-Gitea-OTP", totp)
	}
	client := &http.Client{Timeout: httpTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("创建 MCP 令牌请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("创建 MCP 令牌失败（HTTP %d）：请检查用户名、密码、--totp 验证码及令牌名；不支持密码认证的站点请用 --token-file 录入专用令牌", response.StatusCode)
	}
	var payload struct {
		Token string `json:"sha1"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("解析 MCP 令牌响应失败")
	}
	if payload.Token == "" {
		return "", fmt.Errorf("Gitea 未返回新建令牌")
	}
	return payload.Token, nil
}
