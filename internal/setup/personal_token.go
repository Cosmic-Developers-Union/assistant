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

// 个人令牌端点（/api/v1/users/{user}/tokens）只接受账号自己的 Basic Auth：
// 管理员令牌与 OAuth 令牌都会被拒，因此这里的每条请求都现场用密码认证，密码
// 只用于本次请求、不落盘。TOTP 走 X-Gitea-OTP 头。

type userTokenInfo struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	TokenLastEight string `json:"token_last_eight"`
}

// validateUserTokenHost 校验站点地址可用作 API 根。
func validateUserTokenHost(host string) error {
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("无效的 Gitea 站点地址")
	}
	return nil
}

// userTokenRequest 以 Basic Auth 调用令牌端点；out 非空时解析 JSON 响应。
// 错误信息只带状态码，不回显服务端响应体（避免把凭据/提示语泄进日志）。
func userTokenRequest(
	ctx context.Context,
	method, host, path, user, password, totp string,
	body any,
	out any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(host, "/")+path, reader)
	if err != nil {
		return err
	}
	request.SetBasicAuth(user, password)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if totp != "" {
		request.Header.Set("X-Gitea-OTP", totp)
	}
	client := &http.Client{Timeout: httpTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &httpError{Method: method, Path: path, Status: response.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("解析 %s 响应失败", path)
	}
	return nil
}

// ListUserTokens 列出账号自己的令牌（令牌值不可回读，只有末 8 位）。
func ListUserTokens(ctx context.Context, host, user, password, totp string) ([]userTokenInfo, error) {
	var result []userTokenInfo
	for page := 1; ; page++ {
		var batch []userTokenInfo
		path := fmt.Sprintf("/api/v1/users/%s/tokens?page=%d&limit=50", url.PathEscape(user), page)
		if err := userTokenRequest(ctx, http.MethodGet, host, path, user, password, totp, nil, &batch); err != nil {
			return nil, err
		}
		result = append(result, batch...)
		if len(batch) < 50 {
			return result, nil
		}
	}
}

// CreateUserToken 用账号自己的密码创建令牌，返回明文（仅此一次可见）。
func CreateUserToken(
	ctx context.Context,
	host, user, password, totp, name string,
	scopes []string,
) (string, error) {
	var payload struct {
		Token string `json:"sha1"`
	}
	path := "/api/v1/users/" + url.PathEscape(user) + "/tokens"
	err := userTokenRequest(ctx, http.MethodPost, host, path, user, password, totp,
		map[string]any{"name": name, "scopes": scopes}, &payload)
	if err != nil {
		return "", fmt.Errorf("创建令牌失败：%w；请检查用户名、密码、--totp 验证码及令牌名，"+
			"不支持密码认证的站点请用 --token-file 录入专用令牌", err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("Gitea 未返回新建令牌")
	}
	return payload.Token, nil
}

// DeleteUserToken 删除账号自己的令牌（不存在时视为已删除）。
func DeleteUserToken(ctx context.Context, host, user, password, totp string, id int64) error {
	path := fmt.Sprintf("/api/v1/users/%s/tokens/%d", url.PathEscape(user), id)
	return userTokenRequest(ctx, http.MethodDelete, host, path, user, password, totp, nil, nil)
}

// EnsureUserToken 保证账号上只有一条名为 name 的用途令牌：先删除同名旧令牌
// （Gitea 不回读令牌值，无法原地续期，只能轮换），再用给定 scope 新建。
// 返回新令牌与轮换掉的同名旧令牌数。
//
// 复用判断不在这里：调用方先用已存的令牌做身份校验，校验通过就完全不调用本函数
// （重复登录不会产生新令牌）。
func EnsureUserToken(
	ctx context.Context,
	host, user, password, totp, name string,
	scopes []string,
) (token string, replaced int, err error) {
	if err := validateUserTokenHost(host); err != nil {
		return "", 0, err
	}
	if user == "" || password == "" || name == "" || len(scopes) == 0 {
		return "", 0, fmt.Errorf("创建令牌需要用户名、密码、令牌名与 scope")
	}
	tokens, err := ListUserTokens(ctx, host, user, password, totp)
	if err != nil {
		return "", 0, fmt.Errorf("列出 @%s 的令牌失败：%w", user, err)
	}
	for _, item := range tokens {
		if item.Name != name {
			continue
		}
		if err := DeleteUserToken(ctx, host, user, password, totp, item.ID); err != nil {
			return "", 0, fmt.Errorf("删除同名旧令牌 %s 失败：%w", name, err)
		}
		replaced++
	}
	token, err = CreateUserToken(ctx, host, user, password, totp, name, scopes)
	if err != nil {
		return "", replaced, err
	}
	return token, replaced, nil
}
