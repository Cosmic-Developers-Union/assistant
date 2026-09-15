package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"assistant/internal/claudecfg"
)

// 自检只发一个 max_tokens=1 的最小请求：够区分「凭据/端点对得上」与「被拒绝」，
// 又不产生实质费用。
const (
	defaultBaseURL     = "https://api.anthropic.com"
	checkTimeout       = 15 * time.Second
	checkModelFallback = "claude-3-5-haiku-latest"
	checkBodyLimit     = 200
)

// CredentialCheck 是一次凭据自检的结果。
type CredentialCheck struct {
	// BaseURL 是实际请求的端点（provider 没给 ANTHROPIC_BASE_URL 时为官方端点）
	BaseURL string
	// Model 是自检用的模型名（取 provider 的模型覆盖，缺省一个便宜的官方档）
	Model string
	// Status 是 HTTP 状态码；0 表示请求没发出去
	Status int
	// Message 是服务端返回的简短文本（错误原因）
	Message string
	// Err 是网络层错误（DNS/连接/超时）
	Err error
}

// OK 表示凭据被端点接受。
func (c CredentialCheck) OK() bool { return c.Status >= 200 && c.Status < 300 }

// Describe 返回一行可读结论（日志/校验输出用）。
func (c CredentialCheck) Describe() string {
	switch {
	case c.Err != nil:
		return fmt.Sprintf("无法连接 %s：%v", c.BaseURL, c.Err)
	case c.OK():
		return fmt.Sprintf("端点连通（%s，模型 %s）", c.BaseURL, c.Model)
	default:
		return fmt.Sprintf("端点 %s 返回 HTTP %d：%s", c.BaseURL, c.Status, c.Message)
	}
}

// CredentialHint 是自检失败时的排查提示：端点与密钥不配套是最常见的原因
// （例如 MiniMax 国内账号的 key 在国际端点上必然 401）。
const CredentialHint = "检查 api_key 与端点是否配套（如 MiniMax 国内账号要用 minimax-cn / api.minimaxi.com）、密钥是否过期；可用 assistant validate --online 复测"

// CheckCredential 用最小请求验证 provider 的凭据与端点是否真的可用。daemon 启动时
// 跑一次，能把「密钥/端点不对 → 每条消息静默重试几分钟」变成一行明确告警。
func CheckCredential(ctx context.Context, overrides claudecfg.Overrides) CredentialCheck {
	if ctx == nil {
		ctx = context.Background()
	}
	result := CredentialCheck{
		BaseURL: strings.TrimRight(firstNonEmpty(overrides.Env["ANTHROPIC_BASE_URL"], defaultBaseURL), "/"),
		Model: firstNonEmpty(
			overrides.Env["ANTHROPIC_MODEL"],
			overrides.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"],
			overrides.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"],
			checkModelFallback,
		),
	}
	apiKey, authToken := credentialTokens(overrides)
	if apiKey == "" && authToken == "" {
		result.Err = errors.New("没有 api_key / auth_token")
		return result
	}
	payload, err := json.Marshal(map[string]any{
		"model":      result.Model,
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		result.Err = err
		return result
	}
	requestCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		result.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		result.Err = err
		return result
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	if apiKey != "" {
		request.Header.Set("x-api-key", apiKey)
	} else {
		request.Header.Set("authorization", "Bearer "+authToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		result.Err = err
		return result
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, checkBodyLimit*4))
	result.Status = response.StatusCode
	result.Message = compactMessage(string(body))
	return result
}

// credentialTokens 按会话生效顺序取出密钥：provider env 优先，其次进程环境（与
// claudecfg.CredentialSource 同口径）。api key 走 x-api-key，auth token 走 Bearer。
func credentialTokens(overrides claudecfg.Overrides) (apiKey, authToken string) {
	apiKey = firstNonEmpty(overrides.Env["ANTHROPIC_API_KEY"], os.Getenv("ANTHROPIC_API_KEY"))
	authToken = firstNonEmpty(overrides.Env["ANTHROPIC_AUTH_TOKEN"], os.Getenv("ANTHROPIC_AUTH_TOKEN"))
	return apiKey, authToken
}

// compactMessage 把响应体压成一行短文本（日志里不刷屏）。
func compactMessage(body string) string {
	text := strings.Join(strings.Fields(strings.TrimSpace(body)), " ")
	if text == "" {
		return "（空响应）"
	}
	if runes := []rune(text); len(runes) > checkBodyLimit {
		return string(runes[:checkBodyLimit]) + "…"
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
