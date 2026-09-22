package daemon

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"assistant/internal/telegram"
)

// DefaultTelegramSplitLimit 是 Telegram 回复切块的缺省上限（平台文本上限 4096）。
const DefaultTelegramSplitLimit = 4000

// TelegramChannelConfig 是 Telegram 通道的适配配置（通道私有部分；通用部分见
// ChannelConfig）。
type TelegramChannelConfig struct {
	// Name 是通道实例键（会话映射层的 transport 键）：空 = "telegram"；多开
	// 同平台 bot 时为 "telegram/<name>"
	Name        string
	BotToken    string
	APIBaseURL  string
	AdminUsers  []string
	SplitLimit  int
	HTTPClient  *http.Client
}

// TelegramChannel 是 Telegram 通道：getUpdates 长轮询收消息（出站连接）、
// sendMessage 被动回复、sendChatAction 显示「正在输入」。
type TelegramChannel struct {
	client     *telegram.Client
	name       string
	adminUsers []string
	splitLimit int
	log        func(string, ...any)

	// pending 只在 Receive（通用桥的单消费协程）里访问：一批更新待逐条交付
	pending []telegram.Message
}

// NewTelegramChannel 创建 Telegram 通道。config 需先经 instances.Normalize
// （api_base_url 已填缺省）。
func NewTelegramChannel(config TelegramChannelConfig, log func(string, ...any)) *TelegramChannel {
	if log == nil {
		log = func(string, ...any) {}
	}
	name := strings.TrimSpace(config.Name)
	if name == "" {
		name = "telegram"
	}
	limit := config.SplitLimit
	if limit <= 0 {
		limit = DefaultTelegramSplitLimit
	}
	return &TelegramChannel{
		client: telegram.NewClient(telegram.Config{
			BotToken:   config.BotToken,
			APIBaseURL: config.APIBaseURL,
			HTTPClient: config.HTTPClient,
			Log:        log,
		}),
		name:       name,
		adminUsers: config.AdminUsers,
		splitLimit: limit,
		log:        log,
	}
}

// Name 实现 Channel：会话映射层的 transport 键。
func (c *TelegramChannel) Name() string { return c.name }

// SplitLimit 实现 Channel。
func (c *TelegramChannel) SplitLimit() int { return c.splitLimit }

// Allowed 实现 Channel："*" 通配 → 数字用户 id 白名单 → 全部拒绝。群聊按
// 发送者 id 判定；新用户的 id 在「忽略用户」日志里可见，加进白名单重启生效。
func (c *TelegramChannel) Allowed(user string) (bool, string) {
	if strings.TrimSpace(user) == "" {
		return false, "用户标识为空"
	}
	if slices.Contains(c.adminUsers, "*") {
		return true, ""
	}
	if len(c.adminUsers) > 0 {
		if slices.Contains(c.adminUsers, user) {
			return true, ""
		}
		return false, "白名单外用户"
	}
	return false, "未配置 admin_users（配置 [\"*\"] 可放开所有人）"
}

// Receive 实现 Channel：长轮询 getUpdates（客户端内部推进确认水位），一次批量
// 取到的消息逐条交付；非文本/无发送者的更新直接丢弃。
func (c *TelegramChannel) Receive(ctx context.Context) (Inbound, error) {
	if len(c.pending) > 0 {
		next := c.pending[0]
		c.pending = c.pending[1:]
		return c.inbound(next), nil
	}
	for !ctxDone(ctx) {
		updates, err := c.client.GetUpdates(ctx)
		if err != nil {
			return nil, err
		}
		for _, update := range updates {
			if update.Message == nil || update.Message.From == nil {
				continue // 回调按钮/频道帖等非消息更新不关心
			}
			if strings.TrimSpace(update.Message.Text) == "" {
				continue
			}
			c.pending = append(c.pending, *update.Message)
		}
		if len(c.pending) == 0 {
			continue
		}
		next := c.pending[0]
		c.pending = c.pending[1:]
		return c.inbound(next), nil
	}
	return nil, ctx.Err()
}

// inbound 把 Telegram 消息包装成通用入站消息（回复绑定该消息的 chat id）。
func (c *TelegramChannel) inbound(message telegram.Message) Inbound {
	return &telegramInbound{
		channel: c,
		userID:  formatTelegramID(message.From.ID),
		chatID:  formatTelegramID(message.Chat.ID),
		message: message,
	}
}

// telegramInbound 是 Inbound 的 Telegram 实现。
type telegramInbound struct {
	channel *TelegramChannel
	message telegram.Message
	userID  string
	chatID  string
}

func (m *telegramInbound) Transport() string { return m.channel.Name() }

// User 是发送者数字 id（白名单与会话键都用它；群聊里每个成员各自一套会话）。
func (m *telegramInbound) User() string { return m.userID }

func (m *telegramInbound) Text() string { return m.message.Text }

// Reply 发送一块回复（私聊回到用户、群聊回到群）。
func (m *telegramInbound) Reply(ctx context.Context, text string) error {
	return m.channel.client.SendMessage(ctx, m.chatID, text)
}

// Typing 显示「正在输入」（Telegram 无关闭语义，off 为空操作）。
func (m *telegramInbound) Typing(ctx context.Context, on bool) {
	if !on {
		return
	}
	_ = m.channel.client.SendTyping(ctx, m.chatID)
}

func formatTelegramID(id int64) string {
	return strings.TrimSpace(telegram.FormatChatID(id))
}
