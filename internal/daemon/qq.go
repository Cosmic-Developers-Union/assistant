package daemon

import (
	"context"
	"encoding/json" // 仅用 RawMessage（json/v2 不提供）
	jsonv2 "encoding/json/v2"
	"slices"
	"strings"
	"sync"

	"assistant/internal/qq"
)

// DefaultQQSplitLimit 是 QQ 回复切块的缺省上限（平台对 content 长度有限制）。
const DefaultQQSplitLimit = 1000

// qqEventQueueSize 是网关事件 → 通用桥的背压队列上限：网关回调在队列满时阻塞，
// 心跳不受影响。
const qqEventQueueSize = 64

// qqMsgSeqCacheLimit 是 msg_seq 计数的缓存上限：msg_id 只有 15 分钟被动窗口，
// 超限整体清空（最坏情况是极旧消息的重发被平台去重）。
const qqMsgSeqCacheLimit = 256

// QQChannelConfig 是 QQ 通道的装配配置（形态对齐 WeixinChannelConfig/
// TelegramChannelConfig）。
type QQChannelConfig struct {
	AppID      string
	AppSecret  string
	APIBaseURL string
	Sandbox    bool
	AdminUsers []string
	SplitLimit int
}

// QQChannel 是 QQ 开放平台机器人通道：WebSocket 网关收事件（出站连接）、
// REST 被动回复；准入靠 openid 白名单，群聊事件平台只在被 @ 时派发。
type QQChannel struct {
	name       string
	client     *qq.Client
	gateway    *qq.Gateway
	adminUsers []string
	splitLimit int
	log        func(string, ...any)

	// events 是网关事件 → Receive 的投递队列
	events chan Inbound
	// runCtx 在 Start 时记录：队列满时 onEvent 借它感知关闭，避免永久阻塞
	runCtx context.Context

	// dead 在网关因不可恢复错误（如凭据被平台拒绝）退出时关闭，Receive 借它
	// 把 fatalErr 交给通用桥；正常退出（ctx 取消）不走这条路。
	dead     chan struct{}
	fatalErr error

	// seqMu 保护 msg_seq 计数（同一 msg_id 的多块回复需要递增序号）
	seqMu  sync.Mutex
	msgSeq map[string]int
}

// NewQQChannel 创建 QQ 通道。config.APIBaseURL 需先填缺省（instances.Normalize
// 已做）；token 端点用官方地址。key 是通道实例键（空 = "qq"；多开同平台
// 机器人时为 "qq/<name>"）。
func NewQQChannel(config QQChannelConfig, key string, debug bool, log func(string, ...any)) *QQChannel {
	client := qq.NewClient(qq.Config{
		AppID:      config.AppID,
		AppSecret:  config.AppSecret,
		APIBaseURL: config.APIBaseURL,
		Debug:      debug,
		Log:        log,
	})
	return newQQChannelWithClient(config, client, key, debug, log)
}

// newQQChannelWithClient 是可注入客户端的内部构造（测试用假 API）。
func newQQChannelWithClient(config QQChannelConfig, client *qq.Client, key string, debug bool, log func(string, ...any)) *QQChannel {
	if log == nil {
		log = func(string, ...any) {}
	}
	if strings.TrimSpace(key) == "" {
		key = "qq"
	}
	limit := config.SplitLimit
	if limit <= 0 {
		limit = DefaultQQSplitLimit
	}
	return &QQChannel{
		name:       strings.TrimSpace(key),
		client:     client,
		gateway:    qq.NewGateway(client, qq.IntentGroupAndC2CEvent),
		adminUsers: config.AdminUsers,
		splitLimit: limit,
		log:        log,
		events:     make(chan Inbound, qqEventQueueSize),
		dead:       make(chan struct{}),
		msgSeq:     map[string]int{},
	}
}

// Name 实现 Channel：会话映射层的 transport 键。
func (c *QQChannel) Name() string { return c.name }

// SplitLimit 实现 Channel。
func (c *QQChannel) SplitLimit() int { return c.splitLimit }

// Allowed 实现 Channel："*" 通配 → 白名单 → 全部拒绝（QQ 没有登录者兜底：
// 拿不到凭据就什么人都收不到）。
func (c *QQChannel) Allowed(user string) (bool, string) {
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
	return false, "未配置 qq.admin_users（配置 [\"*\"] 可放开所有人）"
}

// Start 实现 starter：后台拉起网关（重连内建），事件经队列交付；网关因不可
// 恢复错误退出时记录原因并关闭 dead，让 Receive 把错误交给通用桥。
func (c *QQChannel) Start(ctx context.Context) error {
	c.runCtx = ctx
	go func() {
		if err := c.gateway.Run(ctx, c.onEvent); err != nil {
			c.fatalErr = err
			close(c.dead)
			return
		}
		c.log("QQ 网关已退出")
	}()
	return nil
}

// Receive 实现 Channel：从事件队列取下一条；网关致命退出时返回该错误。
func (c *QQChannel) Receive(ctx context.Context) (Inbound, error) {
	select {
	case message := <-c.events:
		return message, nil
	case <-c.dead:
		return nil, c.fatalErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// onEvent 处理网关派发：只取两类文本消息事件，归一化后投递队列。
func (c *QQChannel) onEvent(eventType string, payload json.RawMessage) {
	switch eventType {
	case qq.EventC2CMessage, qq.EventGroupAtMessage:
	default:
		return // intents 只订阅了消息事件；其余类型（如好友添加）不关心
	}
	var event qqMessageEvent
	if err := jsonv2.Unmarshal(payload, &event); err != nil {
		c.log("解析 QQ 消息事件失败：%v", err)
		return
	}
	user := event.user()
	if user == "" || event.ID == "" {
		c.log("QQ 消息事件缺少用户或 msg_id（忽略）：%+v", event)
		return
	}
	text := cleanQQContent(event.Content)
	if strings.TrimSpace(text) == "" {
		return
	}
	inbound := &qqInbound{
		channel:     c,
		userOpenID:  user,
		groupOpenID: event.GroupOpenID,
		msgID:       event.ID,
		text:        text,
	}
	select {
	case c.events <- inbound:
	case <-c.runCtx.Done():
	}
}

// qqMessageEvent 是 C2C_MESSAGE_CREATE / GROUP_AT_MESSAGE_CREATE 的公共载荷：
// 私聊带 author.user_openid，群聊带 group_openid + author.member_openid。
type qqMessageEvent struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	GroupOpenID string `json:"group_openid,omitempty"`
	Author      struct {
		ID           string `json:"id"`
		UserOpenID   string `json:"user_openid"`
		MemberOpenID string `json:"member_openid"`
	} `json:"author"`
}

// user 返回通道内用户标识：群聊用成员 openid，私聊用用户 openid。
func (e *qqMessageEvent) user() string {
	if e.GroupOpenID != "" {
		return firstNonEmptyString(e.Author.MemberOpenID, e.Author.ID)
	}
	return firstNonEmptyString(e.Author.UserOpenID, e.Author.ID)
}

// cleanQQContent 清理平台投递的文本：去首尾空白与残留的 @ 提及前缀。
func cleanQQContent(content string) string {
	text := strings.TrimSpace(content)
	for range 2 {
		switch {
		case strings.HasPrefix(text, "<@!"), strings.HasPrefix(text, "<@"):
			_, rest, found := strings.Cut(text, ">")
			if !found {
				return text
			}
			text = strings.TrimSpace(rest)
		case strings.HasPrefix(text, "@"):
			_, rest, found := strings.Cut(text, " ")
			if !found {
				return text
			}
			text = strings.TrimSpace(rest)
		default:
			return text
		}
	}
	return text
}

// qqInbound 是 Inbound 的 QQ 实现：回复绑定该条消息的 msg_id/msg_seq。
type qqInbound struct {
	channel     *QQChannel
	userOpenID  string
	groupOpenID string
	msgID       string
	text        string
}

func (m *qqInbound) Transport() string { return m.channel.Name() }

func (m *qqInbound) User() string { return m.userOpenID }

func (m *qqInbound) Text() string { return m.text }

// Reply 发一块回复：被动消息必须带收到消息的 msg_id（15 分钟窗口），msg_seq
// 按 msg_id 递增（平台按 msg_id+msg_seq 去重）。
func (m *qqInbound) Reply(ctx context.Context, text string) error {
	m.channel.seqMu.Lock()
	next := m.channel.msgSeq[m.msgID] + 1
	m.channel.msgSeq[m.msgID] = next
	if len(m.channel.msgSeq) > qqMsgSeqCacheLimit {
		clear(m.channel.msgSeq)
	}
	m.channel.seqMu.Unlock()
	if m.groupOpenID != "" {
		return m.channel.client.SendGroupText(ctx, m.groupOpenID, m.msgID, next, text)
	}
	return m.channel.client.SendC2CText(ctx, m.userOpenID, m.msgID, next, text)
}

// Typing 无「正在输入」协议，空实现。
func (m *qqInbound) Typing(context.Context, bool) {}
