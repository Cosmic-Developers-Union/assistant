// Package weixin 实现 Tencent/openclaw-weixin 的 ilink Bot 协议客户端：
// 扫码登录换 Bot token、getUpdates 长轮询收消息、sendMessage 发文本、typing
// 状态与上下线通知。协议见仓库 docs/protocol_zh_CN.md（HTTP JSON + Bearer）。
//
// 只实现对话所需的最小面：文本消息（含语音转写文本）收发；媒体/文件不在范围内。
package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"assistant/internal/instances"
)

// 协议常量。
const (
	appID          = "bot"
	contentType    = "application/json"
	messageTypeBot = 2
	messageDone    = 2
	itemTypeText   = 1
	itemTypeVoice  = 3
	// TypingOn / TypingOff 是 sendTyping 的 status 取值。
	TypingOn  = 1
	TypingOff = 2
)

// Config 是客户端配置。
type Config struct {
	BaseURL        string
	BotToken       string
	BotAgent       string
	ChannelVersion string
	RouteTag       string
	// HTTPClient 可覆盖（测试注入）；缺省 30s 超时（长轮询请求单独设限）
	HTTPClient *http.Client
}

func (c *Config) normalize() {
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = instances.DefaultWeixinBaseURL
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if strings.TrimSpace(c.BotAgent) == "" {
		c.BotAgent = "OpenClaw"
	}
	if strings.TrimSpace(c.ChannelVersion) == "" {
		c.ChannelVersion = "0.0.1"
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
}

// Client 是 ilink Bot API 客户端。
type Client struct {
	config Config
}

// NewClient 创建客户端。
func NewClient(config Config) *Client {
	config.normalize()
	return &Client{config: config}
}

// BaseURL 返回当前 API 根（扫码重定向后可能变化，Login 用）。
func (c *Client) BaseURL() string { return c.config.BaseURL }

// Message 是一条入站消息（只保留对话所需字段）。
type Message struct {
	Seq          int64         `json:"seq"`
	MessageID    int64         `json:"message_id"`
	FromUserID   string        `json:"from_user_id"`
	ToUserID     string        `json:"to_user_id"`
	SessionID    string        `json:"session_id"`
	GroupID      string        `json:"group_id"`
	MessageType  int           `json:"message_type"`
	MessageState int           `json:"message_state"`
	ItemList     []MessageItem `json:"item_list"`
	ContextToken string        `json:"context_token"`
	CreateTimeMS int64         `json:"create_time_ms"`
}

// MessageItem 是消息内容项。
type MessageItem struct {
	Type     int `json:"type"`
	TextItem *struct {
		Text string `json:"text"`
	} `json:"text_item,omitempty"`
	VoiceItem *struct {
		Text string `json:"text"`
	} `json:"voice_item,omitempty"`
}

// Text 提取文本内容：文本消息与语音转写（voice_item.text）都算。
func (m Message) Text() string {
	var parts []string
	for _, item := range m.ItemList {
		switch item.Type {
		case itemTypeText:
			if item.TextItem != nil && strings.TrimSpace(item.TextItem.Text) != "" {
				parts = append(parts, item.TextItem.Text)
			}
		case itemTypeVoice:
			if item.VoiceItem != nil && strings.TrimSpace(item.VoiceItem.Text) != "" {
				parts = append(parts, item.VoiceItem.Text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// Updates 是一次 getUpdates 的结果。
type Updates struct {
	Messages      []Message
	Cursor        string
	LongPollMS    int64
	Ret           int
	ErrCode       int
	ErrMsg        string
	ServerTimeout time.Duration
}

// GetUpdates 长轮询拉取新消息；cursor 为上次响应的 get_updates_buf（首轮空串）。
func (c *Client) GetUpdates(ctx context.Context, cursor string) (Updates, error) {
	body := map[string]any{
		"get_updates_buf": cursor,
		"base_info":       c.baseInfo(),
	}
	var response struct {
		Ret                int       `json:"ret"`
		ErrCode            int       `json:"errcode"`
		ErrMsg             string    `json:"errmsg"`
		Messages           []Message `json:"msgs"`
		Cursor             string    `json:"get_updates_buf"`
		LongPollingTimeout int64     `json:"longpolling_timeout_ms"`
	}
	if err := c.do(ctx, http.MethodPost, "/ilink/bot/getupdates", body, true, &response); err != nil {
		return Updates{}, err
	}
	updates := Updates{
		Messages:   response.Messages,
		Cursor:     response.Cursor,
		LongPollMS: response.LongPollingTimeout,
		Ret:        response.Ret,
		ErrCode:    response.ErrCode,
		ErrMsg:     response.ErrMsg,
	}
	if updates.LongPollMS > 0 {
		updates.ServerTimeout = time.Duration(updates.LongPollMS) * time.Millisecond
	}
	return updates, nil
}

// SendText 发送文本消息；contextToken 用于回复对应会话。
func (c *Client) SendText(ctx context.Context, toUserID, contextToken, text string) error {
	clientID, err := randomClientID()
	if err != nil {
		return err
	}
	message := map[string]any{
		"from_user_id":  "",
		"to_user_id":    toUserID,
		"client_id":     clientID,
		"message_type":  messageTypeBot,
		"message_state": messageDone,
		"item_list": []any{
			map[string]any{"type": itemTypeText, "text_item": map[string]any{"text": text}},
		},
	}
	if contextToken != "" {
		message["context_token"] = contextToken
	}
	body := map[string]any{"msg": message, "base_info": c.baseInfo()}
	var response struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	if err := c.do(ctx, http.MethodPost, "/ilink/bot/sendmessage", body, true, &response); err != nil {
		return err
	}
	if response.Ret != 0 {
		return fmt.Errorf("sendMessage ret=%d %s", response.Ret, response.ErrMsg)
	}
	return nil
}

// GetTypingTicket 获取会话的 typing ticket（输入状态所需）。
func (c *Client) GetTypingTicket(ctx context.Context, userID, contextToken string) (string, error) {
	body := map[string]any{"ilink_user_id": userID, "base_info": c.baseInfo()}
	if contextToken != "" {
		body["context_token"] = contextToken
	}
	var response struct {
		Ret          int    `json:"ret"`
		TypingTicket string `json:"typing_ticket"`
	}
	if err := c.do(ctx, http.MethodPost, "/ilink/bot/getconfig", body, true, &response); err != nil {
		return "", err
	}
	return response.TypingTicket, nil
}

// SendTyping 设置或取消输入状态（status: TypingOn / TypingOff）。
func (c *Client) SendTyping(ctx context.Context, userID, ticket string, status int) error {
	body := map[string]any{
		"ilink_user_id": userID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info":     c.baseInfo(),
	}
	var response struct {
		Ret int `json:"ret"`
	}
	return c.do(ctx, http.MethodPost, "/ilink/bot/sendtyping", body, true, &response)
}

// Notify 发送上下线通知（start=true 为上线）。
func (c *Client) Notify(ctx context.Context, start bool) error {
	path := "/ilink/bot/msg/notifystop"
	if start {
		path = "/ilink/bot/msg/notifystart"
	}
	body := map[string]any{"base_info": c.baseInfo()}
	var response struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, true, &response); err != nil {
		return err
	}
	if response.Ret != 0 {
		return fmt.Errorf("%s ret=%d %s", path, response.Ret, response.ErrMsg)
	}
	return nil
}

func (c *Client) baseInfo() map[string]any {
	return map[string]any{
		"channel_version": c.config.ChannelVersion,
		"bot_agent":       c.config.BotAgent,
	}
}

// do 执行一次 JSON 请求。auth 为 true 时带 Bot 鉴权头（扫码流程不带）。
func (c *Client) do(ctx context.Context, method, path string, body any, auth bool, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.config.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	c.setHeaders(request, auth)
	response, err := c.config.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %s", method, path, response.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: 解析响应: %w", method, path, err)
	}
	return nil
}

// setHeaders 设置协议公共请求头；auth 为 false 时省略 Authorization。
func (c *Client) setHeaders(request *http.Request, auth bool) {
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("iLink-App-Id", appID)
	request.Header.Set("iLink-App-ClientVersion", EncodeClientVersion(c.config.ChannelVersion))
	request.Header.Set("X-WECHAT-UIN", randomUIN())
	if tag := strings.TrimSpace(c.config.RouteTag); tag != "" {
		request.Header.Set("SKRouteTag", tag)
	}
	if auth {
		request.Header.Set("AuthorizationType", "ilink_bot_token")
		request.Header.Set("Authorization", "Bearer "+c.config.BotToken)
	}
}

// EncodeClientVersion 把 x.y.z 版本编码为协议要求的 0x00MMNNPP 十进制字符串。
func EncodeClientVersion(version string) string {
	parts := strings.Split(strings.TrimSpace(version), ".")
	number := func(index int) uint32 {
		if index >= len(parts) {
			return 0
		}
		value, err := strconv.Atoi(parts[index])
		if err != nil || value < 0 {
			return 0
		}
		if value > 255 {
			value = 255
		}
		return uint32(value)
	}
	encoded := number(0)<<16 | number(1)<<8 | number(2)
	return strconv.FormatUint(uint64(encoded), 10)
}

// randomUIN 是随机 uint32 十进制字符串的 base64 编码。
func randomUIN() string {
	buffer := make([]byte, 4)
	_, _ = rand.Read(buffer)
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(buffer)), 10)))
}

// randomClientID 生成消息 client_id。
func randomClientID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "assistant-" + hex.EncodeToString(buffer), nil
}

// SplitText 把长回复按上限切块（按 rune，尽量在换行处切）。
func SplitText(text string, limit int) []string {
	runes := []rune(strings.TrimSpace(text))
	if limit <= 0 || len(runes) <= limit {
		return []string{string(runes)}
	}
	var chunks []string
	for len(runes) > limit {
		cut := limit
		for index := limit; index > limit/2; index-- {
			if runes[index] == '\n' {
				cut = index
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, strings.TrimSpace(string(runes)))
	}
	return chunks
}
