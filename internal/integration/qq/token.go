package qq

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tokenRefreshAhead 是 token 过期前提前刷新的余量（避免边界竞态）。
const tokenRefreshAhead = 5 * time.Minute

// FatalError 是重试无法解决的凭据/配置类错误：AppID/AppSecret 被平台拒绝、
// 请求参数非法等。网关与通道层收到它应停止重连——修正配置前重试只会刷屏。
type FatalError struct {
	Stage   string // 失败阶段，如 "换取 access token"
	Status  int    // HTTP 状态码（0 = 2xx 但业务码报错）
	Code    string // 平台业务码（如 "100007"；未知形态为空）
	Message string // 平台 message（如 "appid invalid"）
	Body    string // 截断后的原始响应（诊断用）
}

func (e *FatalError) Error() string {
	var builder strings.Builder
	builder.WriteString(e.Stage + "被平台拒绝")
	switch {
	case e.Status != 0:
		fmt.Fprintf(&builder, "（HTTP %d）", e.Status)
	case e.Code != "":
		fmt.Fprintf(&builder, "（code %s：%s）", e.Code, e.Message)
	case e.Message != "":
		fmt.Fprintf(&builder, "（%s）", e.Message)
	}
	if e.Body != "" {
		builder.WriteString("：" + e.Body)
	}
	builder.WriteString("。核对 q.qq.com 的 AppID/AppSecret；若 config.json 里写的是 $VAR 引用，检查变量已在 config.json 同目录的 .env 或进程环境中定义")
	return builder.String()
}

// Fatal 标记错误不可重试：daemon 通道层据此停止该通道而不是退避重试。
func (e *FatalError) Fatal() bool { return true }

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
	// 官方端点只接受 JSON body（{"appId","clientSecret"}）；发成表单会被解析成
	// 空 appId，平台返回 {"code":100007,"message":"appid invalid"}——凭据明明
	// 正确也会报 invalid appid。
	requestBody, err := json.Marshal(struct {
		AppID        string `json:"appId"`
		ClientSecret string `json:"clientSecret"`
	}{AppID: strings.TrimSpace(m.appID), ClientSecret: m.appSecret})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", defaultUserAgent)
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
		// 4xx（限流除外）是凭据/参数被拒：重试无意义，标记致命让通道停止。
		if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
			return "", &FatalError{Stage: "换取 access token", Status: response.StatusCode, Body: truncateBody(data)}
		}
		return "", fmt.Errorf("获取 access token 失败：%s（检查 AppID/AppSecret 与开放平台 IP 白名单）", response.Status)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("解析 access token 响应: %w", err)
	}
	token, _ := payload["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		// 2xx 但业务码报错（如 {"code":100007,"message":"appid invalid"}）：
		// 平台拒绝凭据，同样致命。
		if code, message, rejected := platformError(payload); rejected {
			return "", &FatalError{Stage: "换取 access token", Code: code, Message: message, Body: truncateBody(data)}
		}
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

// platformError 从 token 端点响应里提取平台业务错误（code/message 任一存在
// 即算，如 {"code":100007,"message":"appid invalid"}）；code 容忍数字或字符串。
func platformError(payload map[string]any) (code, message string, rejected bool) {
	rawCode, hasCode := payload["code"]
	rawMessage, hasMessage := payload["message"]
	if !hasCode && !hasMessage {
		return "", "", false
	}
	switch value := rawCode.(type) {
	case string:
		code = value
	case float64:
		code = strconv.FormatFloat(value, 'f', -1, 64)
	}
	message, _ = rawMessage.(string)
	return code, message, true
}
