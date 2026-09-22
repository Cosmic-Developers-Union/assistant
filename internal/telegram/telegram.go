// Package telegram 是 Telegram Bot API 的最小客户端：长轮询收消息（getUpdates，
// 出站连接无需公网 IP 或 webhook）、sendMessage 回复、sendChatAction 显示
// 「正在输入」。协议要点：
//
//   - 鉴权：@BotFather 发放的 bot token，所有方法走 POST /bot<token>/<方法>。
//   - 收消息：getUpdates?offset=<last_id+1>&timeout=<秒> 长轮询；返回后把
//     offset 推进到最大 update_id+1 即确认消费。断电重启会重放最近未确认的
//     消息（claude 会话 --resume，重复回复无害）。
//   - 回复：sendMessage {chat_id, text}，纯文本；回复私聊 chat_id=用户 id，
//     回复群聊 chat_id=群 id。文本上限 4096 字符（切块由通道层负责）。
//   - 群聊事件：bot 需关闭隐私模式（BotFather /setprivacy）才能收到全部群消息，
//     否则只有 @ 提与回复可见。
//
// 本包只做协议；通道适配（会话映射、准入、切块）在 internal/daemon。
package telegram

import (
	"bytes"
	"context"
	"encoding/json" // 仅用 RawMessage（json/v2 不提供）
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultAPIBaseURL 是 Telegram Bot API 的默认地址；被墙环境可换成自建反代。
const DefaultAPIBaseURL = "https://api.telegram.org"

// DefaultPollTimeout 是长轮询挂起时长（服务器有新消息会提前返回）。
const DefaultPollTimeout = 30 * time.Second

// Config 是 Telegram 客户端配置。
type Config struct {
	BotToken   string
	APIBaseURL string // 缺省 DefaultAPIBaseURL（自建反代时覆盖）
	HTTPClient *http.Client
	Log        func(string, ...any)
	Debug      bool
}

// Client 是 Telegram Bot API 客户端。
type Client struct {
	config Config
	log    func(string, ...any)
	// offset 是 getUpdates 的确认水位（最大 update_id+1）；仅内存态，重启后
	// 最近未确认消息会被重放
	offset int64
}

// NewClient 创建客户端。
func NewClient(config Config) *Client {
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = DefaultAPIBaseURL
	}
	return &Client{config: config, log: config.Log}
}

// User 是消息发送者。
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

// Chat 是会话（私聊 private / 群 group / 超级群 supergroup）。
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type,omitempty"`
}

// Message 是一条文本消息（非文本消息 Text 为空，通道层跳过）。
type Message struct {
	MessageID int64 `json:"message_id"`
	From      *User `json:"from,omitempty"`
	Chat      Chat  `json:"chat"`
	Text      string `json:"text,omitempty"`
}

// Update 是一次更新（只关心 message）。
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Me 是 getMe 返回的机器人身份（自检用）。
type Me struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

// GetMe 实测 token（凭据自检）。
func (c *Client) GetMe(ctx context.Context) (Me, error) {
	var me Me
	if err := c.call(ctx, "getMe", nil, &me); err != nil {
		return Me{}, err
	}
	return me, nil
}

// GetUpdates 长轮询一批更新；客户端内部推进 offset（确认消费）。
func (c *Client) GetUpdates(ctx context.Context) ([]Update, error) {
	body := map[string]any{"timeout": int(DefaultPollTimeout.Seconds()), "offset": c.offset + 1}
	var updates []Update
	if err := c.call(ctx, "getUpdates", body, &updates); err != nil {
		return nil, err
	}
	for _, update := range updates {
		if update.UpdateID > c.offset {
			c.offset = update.UpdateID
		}
	}
	return updates, nil
}

// SendMessage 向会话发送一条纯文本。
func (c *Client) SendMessage(ctx context.Context, chatID, text string) error {
	return c.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// SendTyping 显示「正在输入」（5 秒内有效，尽力而为）。
func (c *Client) SendTyping(ctx context.Context, chatID string) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}

// call 调一个 Bot API 方法：POST JSON，响应按 {ok, result, description} 解包。
func (c *Client) call(ctx context.Context, method string, body map[string]any, out any) error {
	endpoint := c.config.APIBaseURL + "/bot" + c.config.BotToken + "/" + method
	var reader io.Reader
	if body != nil {
		data, err := jsonv2.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s: 读取响应: %w", method, err)
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description,omitempty"`
		Result      json.RawMessage `json:"result,omitempty"`
	}
	if err := jsonv2.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("%s: 解析响应（HTTP %d）: %w", method, response.StatusCode, err)
	}
	if !envelope.OK {
		return fmt.Errorf("%s 失败（HTTP %d）：%s", method, response.StatusCode, envelope.Description)
	}
	if out != nil && len(envelope.Result) > 0 {
		if err := jsonv2.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("%s: 解析 result: %w", method, err)
		}
	}
	return nil
}

// FormatChatID 是 chat id 的 API 传参形式（sendMessage 的 chat_id 接受数字）。
func FormatChatID(id int64) string { return strconv.FormatInt(id, 10) }
