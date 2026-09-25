package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/weixin"
)

// messageTypeUser 是 WeixinMessage.message_type 中「用户」的取值（2 是 Bot）。
const messageTypeUser = 1

// DefaultWeixinSplitLimit 是微信回复切块的缺省上限。
const DefaultWeixinSplitLimit = 1800

// WeixinChannelConfig 是微信通道的适配配置（通道私有部分；通用部分见 ChannelConfig）。
type WeixinChannelConfig struct {
	// Name 是通道实例键（会话映射层的 transport 键）：空 = "weixin"；多开
	// 同平台账号时为 "weixin/<name>"
	Name        string
	Weixin      weixin.Config
	// AdminUsers 是允许对话的用户白名单；空时只允许 LoginUserID；含 "*" 放开所有人
	AdminUsers []string
	// LoginUserID 是扫码登录的微信用户
	LoginUserID string
	// SplitLimit 是回复切块上限（缺省 DefaultWeixinSplitLimit）
	SplitLimit int
}

// WeixinChannel 是微信（openclaw ilink）通道：长轮询收消息、SendText 回复、
// typing 票据缓存与上下线通知。
type WeixinChannel struct {
	name        string
	client      *weixin.Client
	adminUsers  []string
	loginUserID string
	splitLimit  int
	typing      *typingCache
	log         func(string, ...any)

	// cursor 与 pending 只在 Receive（通用桥的单消费协程）里访问
	cursor  string
	pending []weixin.Message
}

// NewWeixinChannel 创建微信通道。
func NewWeixinChannel(config WeixinChannelConfig, log func(string, ...any)) *WeixinChannel {
	if log == nil {
		log = func(string, ...any) {}
	}
	limit := config.SplitLimit
	if limit <= 0 {
		limit = DefaultWeixinSplitLimit
	}
	name := strings.TrimSpace(config.Name)
	if name == "" {
		name = "weixin"
	}
	return &WeixinChannel{
		name:        name,
		client:      weixin.NewClient(config.Weixin),
		adminUsers:  config.AdminUsers,
		loginUserID: config.LoginUserID,
		splitLimit:  limit,
		typing:      &typingCache{tickets: map[string]string{}},
		log:         log,
	}
}

// Name 实现 Channel：会话映射层的 transport 键。
func (c *WeixinChannel) Name() string { return c.name }

// SplitLimit 实现 Channel。
func (c *WeixinChannel) SplitLimit() int { return c.splitLimit }

// Allowed 实现 Channel："*" 通配 → 白名单 → 登录者兜底 → 全部拒绝。
func (c *WeixinChannel) Allowed(user string) (bool, string) {
	if strings.TrimSpace(user) == "" {
		return false, "用户标识为空"
	}
	if slices.Contains(c.adminUsers, "*") {
		return true, ""
	}
	if len(c.adminUsers) > 0 {
		if !slices.Contains(c.adminUsers, user) {
			return false, "白名单外用户"
		}
		return true, ""
	}
	if c.loginUserID != "" {
		if user != c.loginUserID {
			return false, "非登录用户"
		}
		return true, ""
	}
	return false, "未配置 weixin.admin_users 且无 login_user_id"
}

// Start 实现 starter：上线通知（尽力而为，失败由通用桥记日志）。
func (c *WeixinChannel) Start(ctx context.Context) error {
	return c.client.Notify(ctx, true)
}

// Close 实现 closer：下线通知（尽力而为）。
func (c *WeixinChannel) Close(ctx context.Context) {
	if err := c.client.Notify(ctx, false); err != nil {
		c.log("通知下线失败（忽略）：%v", err)
	}
}

// Receive 实现 Channel：长轮询 ilink。会话被暂停（ret=-14）时挂起 1 小时后
// 重试；空轮询立即再取；一次批量取到的消息逐条交付。
func (c *WeixinChannel) Receive(ctx context.Context) (Inbound, error) {
	if len(c.pending) > 0 {
		next := c.pending[0]
		c.pending = c.pending[1:]
		return c.inbound(next), nil
	}
	for !ctxDone(ctx) {
		updates, err := c.client.GetUpdates(ctx, c.cursor)
		if err != nil {
			return nil, err
		}
		if updates.Ret == -14 || updates.ErrCode == -14 {
			c.log("账号会话被暂停（ret=-14），1 小时后重试")
			sleepCtx(ctx, time.Hour)
			continue
		}
		if updates.Cursor != "" {
			c.cursor = updates.Cursor
		}
		// Bot 自己的消息不是入站对话，直接丢弃（否则 "*" 放开时会自问自答打环）
		for _, message := range updates.Messages {
			if message.MessageType == messageTypeUser {
				c.pending = append(c.pending, message)
			}
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

// inbound 把微信消息包装成通用入站消息（回复绑定该消息的 context_token）。
func (c *WeixinChannel) inbound(message weixin.Message) Inbound {
	return &weixinInbound{channel: c, message: message}
}

// weixinInbound 是 Inbound 的微信实现。
type weixinInbound struct {
	channel *WeixinChannel
	message weixin.Message
}

func (m *weixinInbound) Transport() string { return m.channel.Name() }

func (m *weixinInbound) User() string { return m.message.FromUserID }

func (m *weixinInbound) Text() string { return m.message.Text() }

// Reply 发送一块回复。
func (m *weixinInbound) Reply(ctx context.Context, text string) error {
	return m.channel.client.SendText(ctx, m.message.FromUserID, m.message.ContextToken, text)
}

// Typing 显示/停止「正在输入」：票据走缓存（getConfig 不便宜），任何失败静默
// （typing 状态尽力而为，不影响回复）。
func (m *weixinInbound) Typing(ctx context.Context, on bool) {
	if m.message.MessageType != messageTypeUser {
		return
	}
	ticket, err := m.channel.typing.ticket(ctx, m.channel.client, m.message.FromUserID, m.message.ContextToken)
	if err != nil || ticket == "" {
		return
	}
	status := weixin.TypingOff
	if on {
		status = weixin.TypingOn
	}
	_ = m.channel.client.SendTyping(ctx, m.message.FromUserID, ticket, status)
}

// typingCache 缓存每用户的 typing ticket（getConfig 不便宜）。
type typingCache struct {
	mu      sync.Mutex
	tickets map[string]string
}

func (t *typingCache) ticket(ctx context.Context, client *weixin.Client, userID, contextToken string) (string, error) {
	t.mu.Lock()
	if ticket, ok := t.tickets[userID]; ok && ticket != "" {
		t.mu.Unlock()
		return ticket, nil
	}
	t.mu.Unlock()
	ticket, err := client.GetTypingTicket(ctx, userID, contextToken)
	if err != nil {
		return "", err
	}
	if ticket != "" {
		t.mu.Lock()
		t.tickets[userID] = ticket
		t.mu.Unlock()
	}
	return ticket, nil
}
