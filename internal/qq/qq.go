// Package qq 是 QQ 开放平台机器人（q.qq.com，官方 Bot API v2）的最小客户端：
// WebSocket 网关收事件、REST 发消息。协议要点（对齐官方 openclaw-qqbot 插件
// 所用的 @tencent-connect/qqbot-nodejs SDK）：
//
//   - 鉴权：AppID + AppSecret 以 JSON body POST bots.qq.com/app/getAppAccessToken
//     换 access_token（约 2 小时过期）；REST 与网关鉴权头均为
//     "Authorization: QQBot <token>"（不是 Bearer）。
//   - 事件：WebSocket 网关（出站连接，无需公网 IP），op 2 Identify 订阅
//     GROUP_AND_C2C_EVENT；op 1/op 11 心跳保活；断线 op 6 Resume 续接；
//     op 9 会话失效按 d 布尔值决定能否续接；close 4004 鉴权失败不重试。
//   - 回复：群 POST /v2/groups/{group_openid}/messages、私聊
//     POST /v2/users/{user_openid}/messages，均为被动消息——必须带收到消息的
//     msg_id（15 分钟窗口），msg_seq 区分同一条消息的多次回复（平台按
//     msg_id+msg_seq 去重）。
//
// 本包只做协议；通道适配（会话映射、准入、切块）在 internal/daemon。
package qq

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const (
	// DefaultAPIBaseURL 是 QQ 开放平台机器人 API（v2）的默认地址。
	DefaultAPIBaseURL = "https://api.sgroup.qq.com"
	// DefaultTokenURL 是 access token 端点（固定，不在 APIBaseURL 下）。
	DefaultTokenURL = "https://bots.qq.com/app/getAppAccessToken"
)

// defaultUserAgent 是全部 HTTP 请求（token/REST/网关拨号）携带的 User-Agent。
const defaultUserAgent = "assistant-qqbot"

// authScheme 是平台鉴权头方案：Authorization: QQBot <token>。
const authScheme = "QQBot "

// Config 是 QQ 客户端配置。
type Config struct {
	AppID      string
	AppSecret  string
	APIBaseURL string // 缺省 DefaultAPIBaseURL
	TokenURL   string // 缺省 DefaultTokenURL（测试注入）
	HTTPClient *http.Client
	Log        func(string, ...any)
	Debug      bool
}

// Client 是 QQ 开放平台机器人客户端：token 管理 + REST 发消息 + 网关地址。
type Client struct {
	config Config
	tokens *tokenManager
	log    func(string, ...any)
}

// NewClient 创建客户端。
func NewClient(config Config) *Client {
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = DefaultAPIBaseURL
	}
	if config.TokenURL == "" {
		config.TokenURL = DefaultTokenURL
	}
	return &Client{
		config: config,
		tokens: newTokenManager(config),
		log:    config.Log,
	}
}

// HTTPClient 返回用于网关拨号的 HTTP 客户端。
func (c *Client) HTTPClient() *http.Client {
	if c.config.HTTPClient != nil {
		return c.config.HTTPClient
	}
	return http.DefaultClient
}

// VerifyCredential 实测换取 access token（凭据自检用；无视缓存强制刷新）。
func (c *Client) VerifyCredential(ctx context.Context) error {
	_, err := c.tokens.get(ctx, true)
	return err
}

// GetGateway 拉取 WebSocket 网关地址。
func (c *Client) GetGateway(ctx context.Context) (string, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := c.get(ctx, "/gateway", &payload); err != nil {
		return "", err
	}
	if payload.URL == "" {
		return "", fmt.Errorf("网关地址为空")
	}
	return payload.URL, nil
}

// SendC2CText 发送私聊文本（被动回复：msgID 为收到消息的 id，msgSeq 从 1 递增）。
func (c *Client) SendC2CText(ctx context.Context, userOpenID, msgID string, msgSeq int, text string) error {
	return c.sendMessage(ctx, "/v2/users/"+url.PathEscape(userOpenID)+"/messages", msgID, msgSeq, text)
}

// SendGroupText 发送群聊文本（被动回复，参数含义同 SendC2CText）。
func (c *Client) SendGroupText(ctx context.Context, groupOpenID, msgID string, msgSeq int, text string) error {
	return c.sendMessage(ctx, "/v2/groups/"+url.PathEscape(groupOpenID)+"/messages", msgID, msgSeq, text)
}

func (c *Client) sendMessage(ctx context.Context, path, msgID string, msgSeq int, text string) error {
	body := map[string]any{
		"content": text,
		"msg_type": 0,
		"msg_id":  msgID,
		"msg_seq": msgSeq,
	}
	return c.post(ctx, path, body)
}

// post 发起带鉴权的 POST；遇 401 强制刷新 token 重试一次。
func (c *Client) post(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := range 2 {
		token, err := c.tokens.get(ctx, attempt > 0)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.APIBaseURL+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", authScheme+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", defaultUserAgent)
		status, payload, err := c.do(request)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.log("回复被拒（401，token 可能过期），刷新后重试")
			continue
		}
		if status/100 != 2 {
			return fmt.Errorf("发送消息失败：%d（%s）", status, truncateBody(payload))
		}
		// v2 发消息成功常返回 200/204 空体
		return nil
	}
	return fmt.Errorf("发送消息失败：token 刷新后仍被拒")
}

// get 发起带鉴权的 GET。
func (c *Client) get(ctx context.Context, path string, out any) error {
	for attempt := range 2 {
		token, err := c.tokens.get(ctx, attempt > 0)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.APIBaseURL+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", authScheme+token)
		request.Header.Set("User-Agent", defaultUserAgent)
		status, payload, err := c.do(request)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if status/100 != 2 {
			return fmt.Errorf("请求 %s 失败：%d（%s）", path, status, truncateBody(payload))
		}
		if len(payload) == 0 {
			return nil
		}
		return json.Unmarshal(payload, out)
	}
	return fmt.Errorf("请求 %s 失败：token 刷新后仍被拒", path)
}

func (c *Client) do(request *http.Request) (int, []byte, error) {
	response, err := c.HTTPClient().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, payload, nil
}

func truncateBody(payload []byte) string {
	const limit = 200
	if len(payload) > limit {
		return string(payload[:limit]) + "…"
	}
	return string(payload)
}
