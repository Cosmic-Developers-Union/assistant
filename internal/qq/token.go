package qq

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tokenRefreshAhead 是 token 过期前提前刷新的余量（避免边界竞态）。
const tokenRefreshAhead = 5 * time.Minute

// tokenDefaultTTL 是响应缺 expires_in 时的兜底有效期（官方约 7200 秒）。
const tokenDefaultTTL = 7200 * time.Second

// tokenManager 缓存 access_token 并按需刷新：过期前主动刷新，401 时调用方
// 传 force=true 强制刷新。token 端点返回 {"access_token":"…","expires_in":"7200"}
// （expires_in 官方为字符串，容忍数字）。
type tokenManager struct {
	appID      string
	appSecret  string
	tokenURL   string
	httpClient *http.Client
	log        func(string, ...any)

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newTokenManager(config Config) *tokenManager {
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &tokenManager{
		appID:      config.AppID,
		appSecret:  config.AppSecret,
		tokenURL:   config.TokenURL,
		httpClient: httpClient,
		log:        config.Log,
	}
}

// get 返回有效 token；force 为真时无视缓存重新获取。
func (m *tokenManager) get(ctx context.Context, force bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !force && m.token != "" && time.Now().Before(m.expires) {
		return m.token, nil
	}
	form := url.Values{"appId": {m.appID}, "clientSecret": {m.appSecret}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := m.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("获取 access token: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil {
		return "", fmt.Errorf("读取 access token 响应: %w", err)
	}
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("获取 access token 失败：%s（检查 AppID/AppSecret 与开放平台 IP 白名单）", response.Status)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("解析 access token 响应: %w", err)
	}
	token, _ := payload["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("access token 响应缺少 access_token：%s", truncateBody(data))
	}
	expiry := tokenDefaultTTL
	switch value := any(payload["expires_in"]).(type) {
	case string:
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			expiry = time.Duration(seconds) * time.Second
		}
	case float64:
		if value > 0 {
			expiry = time.Duration(value) * time.Second
		}
	}
	refreshAfter := max(expiry-tokenRefreshAhead, time.Minute)
	m.token = token
	m.expires = time.Now().Add(refreshAfter)
	m.log("已获取 access token（%s 后刷新）", refreshAfter.Round(time.Second))
	return m.token, nil
}
