package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"assistant/internal/weixin"
)

// messageTypeUser 是 WeixinMessage.message_type 中「用户」的取值（2 是 Bot）。
const messageTypeUser = 1

// BridgeConfig 是微信对话桥配置。
type BridgeConfig struct {
	Weixin weixin.Config
	// AdminUsers 是允许对话的用户白名单；空时只允许 LoginUserID
	AdminUsers []string
	// LoginUserID 是扫码登录的微信用户
	LoginUserID string
	Chat        *Chat
	Log         func(string, ...any)
	// Debug 为真时额外记录 typing 票据、消息路由等细节
	Debug bool
	// Concurrency 是同时处理的对话数（缺省 4）
	Concurrency int
}

// RunBridge 常驻消费微信消息：长轮询 → (允许用户) → 稳定 claude 会话 →
// 分块回复。上下线通知、typing 状态尽力而为，失败不影响主流程。
func RunBridge(ctx context.Context, config BridgeConfig) error {
	if config.Chat == nil {
		return fmt.Errorf("对话会话未初始化")
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	client := weixin.NewClient(config.Weixin)
	if err := client.Notify(ctx, true); err != nil {
		config.Log("通知上线失败（忽略）：%v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Notify(stopCtx, false); err != nil {
			config.Log("通知下线失败（忽略）：%v", err)
		}
	}()

	typing := &typingCache{tickets: map[string]string{}}
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	cursor := ""
	backoff := time.Second
	for !ctxDone(ctx) {
		updates, err := client.GetUpdates(ctx, cursor)
		if err != nil {
			if ctxDone(ctx) {
				break
			}
			config.Log("getUpdates 失败：%v（%s 后重试）", err, backoff)
			sleepCtx(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if updates.Ret == -14 || updates.ErrCode == -14 {
			config.Log("账号会话被暂停（ret=-14），1 小时后重试")
			sleepCtx(ctx, time.Hour)
			continue
		}
		if updates.Cursor != "" {
			cursor = updates.Cursor
		}
		for _, message := range updates.Messages {
			if !bridgeAllowed(config, message) {
				continue
			}
			text := message.Text()
			if text == "" {
				continue
			}
			config.Log("收到消息（%s）：%s", message.FromUserID, truncate(singleLine(text), 200))
			waitGroup.Add(1)
			semaphore <- struct{}{}
			go func(message weixin.Message, text string) {
				defer waitGroup.Done()
				defer func() { <-semaphore }()
				handleChatMessage(ctx, config, client, typing, message, text)
			}(message, text)
		}
	}
	waitGroup.Wait()
	return nil
}

// bridgeAllowed 判断消息是否需要处理：只处理用户私聊/群聊文本；白名单外的
// 用户忽略并记录。
func bridgeAllowed(config BridgeConfig, message weixin.Message) bool {
	if message.MessageType != messageTypeUser {
		return false
	}
	if strings.TrimSpace(message.FromUserID) == "" {
		return false
	}
	if len(config.AdminUsers) > 0 {
		if !slices.Contains(config.AdminUsers, message.FromUserID) {
			config.Log("忽略白名单外用户 %s 的消息", message.FromUserID)
			return false
		}
		return true
	}
	if config.LoginUserID != "" {
		if message.FromUserID != config.LoginUserID {
			config.Log("忽略非登录用户 %s 的消息", message.FromUserID)
			return false
		}
		return true
	}
	config.Log("未配置 weixin.admin_users 且无 login_user_id，忽略所有对话消息")
	return false
}

func handleChatMessage(
	ctx context.Context,
	config BridgeConfig,
	client *weixin.Client,
	typing *typingCache,
	message weixin.Message,
	text string,
) {
	// 会话实体由映射层决定：通道（微信）+ 通道内用户标识 → conversation id。
	// 换通道或把多个通道绑到同一会话时，只改映射表。
	conversationID := message.FromUserID
	if mapped, err := config.Chat.ConversationFor("weixin", message.FromUserID); err != nil {
		config.Log("会话映射失败（%s），退回通道用户 id：%v", message.FromUserID, err)
	} else if mapped != "" {
		conversationID = mapped
	}
	if ticket, err := typing.ticket(ctx, client, message.FromUserID, message.ContextToken); err == nil && ticket != "" {
		if err := client.SendTyping(ctx, message.FromUserID, ticket, weixin.TypingOn); err == nil {
			defer func() { _ = client.SendTyping(ctx, message.FromUserID, ticket, weixin.TypingOff) }()
		}
	}
	var reply string
	if restartRequested(text) {
		// 用户明确要求重新开始：只丢会话映射，工作目录保留（用户的文件与历史都还在）
		if resetErr := config.Chat.Reset(conversationID); resetErr != nil {
			config.Log("重置会话失败（%s）：%v", conversationID, resetErr)
		}
		workspace, _ := config.Chat.WorkspaceDir(conversationID)
		config.Log("会话重置（%s）：用户要求重新开始，下一条消息新建 claude 会话", conversationID)
		reply = "已开始新会话：下一条消息会新建 claude 会话（工作目录保留：" + workspace + "）"
	} else {
		var err error
		reply, err = config.Chat.Handle(ctx, conversationID, text)
		if err != nil {
			config.Log("对话失败（%s）：%v", conversationID, err)
			reply = "处理失败：" + err.Error()
		}
	}
	if strings.TrimSpace(reply) == "" {
		reply = "（无回复）"
	}
	config.Log("回复（%s，%d 字）：%s", conversationID, len([]rune(reply)), truncate(singleLine(reply), 200))
	for _, chunk := range weixin.SplitText(reply, 1800) {
		if err := client.SendText(ctx, message.FromUserID, message.ContextToken, chunk); err != nil {
			config.Log("发送回复失败：%v", err)
			return
		}
	}
}

// restartRequested 判断用户是否明确要求「重新开始」：命中则下一条消息新建会
// 话（保留工作目录）。接受斜杠命令与直白说法，避免用户以为换了话题其实还在
// 老会话里。
func restartRequested(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "/new", "/reset", "/clear", "/restart", "重新开始", "新会话", "/新会话", "重置会话":
		return true
	}
	return false
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

func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
