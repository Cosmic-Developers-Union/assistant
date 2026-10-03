// Package integration 定义消息平台的协议边界、准入与切块逻辑，
// RunChat 消费入站消息，runtime 通过 Conversations 提供会话执行。
package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Inbound 是一条入站消息：对话型集成的运行循环通过它读用户与文本、发回复、
// 控制「正在输入」。平台私有的回复上下文（微信 context_token、QQ 的
// msg_id/msg_seq）封装在集成实现里，通用层只认这组方法。
type Inbound interface {
	// Transport 是通道名（与 Integration.Name() 一致）
	Transport() string
	// User 是通道内用户标识
	User() string
	// Text 是文本内容；非文本消息为空，运行循环会跳过
	Text() string
	// Reply 发送一块回复文本
	Reply(ctx context.Context, text string) error
	// Typing 显示/停止「正在输入」（尽力而为，不支持时无操作）
	Typing(ctx context.Context, on bool)
}

// Integration 是所有集成的共同面：名字、准入、切块上限。
//
// 它刻意很小——运行循环不在这一层。对话型集成实现 ChatIntegration（有
// Receive），由 RunChat 驱动。仓库待办由 runtime.Source 单独处理。
type Integration interface {
	// Name 是集成名，同时是会话映射层的 transport 键（weixin/qq/telegram）
	Name() string
	// Allowed 判断用户能否对话（白名单/通配/登录者）；拒绝原因仅用于日志
	Allowed(user string) (allow bool, reason string)
	// SplitLimit 是回复切块的 rune 上限（<=0 表示不切）
	SplitLimit() int
}

// ChatIntegration 是对话型集成：它的运行循环由通用层提供（RunChat），逐条
// 取入站消息并回复。三个聊天平台都实现它。
type ChatIntegration interface {
	Integration
	// Receive 阻塞取下一条入站消息；返回错误时运行循环退避后重试
	Receive(ctx context.Context) (Inbound, error)
}

// Conversations 是消息适配器依赖的会话接口，由 runtime 提供实现。
type Conversations interface {
	// ConversationFor 把「通道 + 通道内用户」映射成会话 id
	ConversationFor(transport, user string) (string, error)
	// Handle 执行一轮对话
	Handle(ctx context.Context, conversationID string, turn Turn) (string, error)
	// Reset 丢弃会话映射（工作目录保留）
	Reset(conversationID string) error
	// WorkspaceDir 返回会话的工作目录
	WorkspaceDir(conversationID string) (string, error)
}

// Turn 是一轮对话请求。
type Turn struct {
	// Transport 是消息来源通道（写入会话元数据）
	Transport string
	// Text 是用户输入
	Text string
}

// starter 是集成可选的启动钩子：运行循环进入消费前调用，失败不致命
// （如微信的上线通知）。
type starter interface {
	Start(ctx context.Context) error
}

// closer 是集成可选的收尾钩子：运行循环退出时以短暂超时调用（如下线通知）。
type closer interface {
	Close(ctx context.Context)
}

// Options 是对话运行循环的配置（每个集成一份；Conversations 跨集成共享）。
type Options struct {
	// Conversations 是共享的对话会话面
	Conversations Conversations
	// Log 输出
	Log func(string, ...any)
	// Debug 为真时输出路由等诊断细节
	Debug bool
	// Concurrency 是该集成同时处理的对话数（缺省 4）
	Concurrency int
}

// RunChat 常驻消费一个对话型集成的消息：接收 → (准入) → 稳定 claude 会话 →
// 分块回复。同一会话内消息由 Conversations 串行，不同会话并发。
func RunChat(ctx context.Context, chat ChatIntegration, options Options) error {
	if chat == nil {
		return fmt.Errorf("通道未初始化")
	}
	if options.Conversations == nil {
		return fmt.Errorf("对话会话未初始化")
	}
	if options.Log == nil {
		options.Log = func(string, ...any) {}
	}
	if startable, ok := chat.(starter); ok {
		if err := startable.Start(ctx); err != nil {
			options.Log("通道 %s 上线通知失败（忽略）：%v", chat.Name(), err)
		}
	}
	if closable, ok := chat.(closer); ok {
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			closable.Close(stopCtx)
		}()
	}
	concurrency := options.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	backoff := time.Second
	for !CtxDone(ctx) {
		message, err := chat.Receive(ctx)
		if err != nil {
			if CtxDone(ctx) {
				break
			}
			// 凭据/配置类致命错误（如 QQ 凭据被平台拒绝）重试无意义：停掉本
			// 通道并报给操作者，其余通道与 daemon 不受影响。错误可能被下层
			// 包装过，用 errors.As 穿透取 Fatal 标记。
			var fatal interface{ Fatal() bool }
			if errors.As(err, &fatal) && fatal.Fatal() {
				options.Log("通道 %s 已停止（配置错误不重试，其余通道不受影响）：%v", chat.Name(), err)
				return err
			}
			options.Log("通道 %s 接收失败：%v（%s 后重试）", chat.Name(), err, backoff)
			SleepCtx(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if message == nil {
			continue
		}
		if allowed, reason := chat.Allowed(message.User()); !allowed {
			options.Log("通道 %s 忽略用户 %s 的消息：%s", chat.Name(), message.User(), reason)
			continue
		}
		text := message.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		options.Log("收到消息（%s/%s）：%s", chat.Name(), message.User(), Truncate(SingleLine(text), 200))
		semaphore <- struct{}{}
		waitGroup.Go(func() {
			defer func() { <-semaphore }()
			HandleInbound(ctx, chat, options, message, text)
		})
	}
	waitGroup.Wait()
	return nil
}

// HandleInbound 处理单条入站消息：会话映射 → 控制命令/对话 → 切块回复。
// 会话实体由映射层决定：通道 + 通道内用户标识 → conversation id；换通道或把
// 多个通道绑到同一会话时，只改映射表。
func HandleInbound(ctx context.Context, chat ChatIntegration, options Options, message Inbound, text string) {
	conversationID := message.User()
	if mapped, err := options.Conversations.ConversationFor(chat.Name(), message.User()); err != nil {
		options.Log("会话映射失败（%s），退回通道用户 id：%v", message.User(), err)
	} else if mapped != "" {
		conversationID = mapped
	}
	message.Typing(ctx, true)
	defer message.Typing(ctx, false)

	var reply string
	if parsed := ParseCommand(text); parsed.Name != "" {
		reply = HandleCommand(options, conversationID, parsed)
	} else {
		var err error
		reply, err = options.Conversations.Handle(ctx, conversationID, Turn{
			Transport: chat.Name(),
			Text:      text,
		})
		if err != nil {
			options.Log("对话失败（%s）：%v", conversationID, err)
			reply = "处理失败：" + err.Error()
		}
	}
	if strings.TrimSpace(reply) == "" {
		reply = "（无回复）"
	}
	options.Log("回复（%s，%d 字）：%s", conversationID, len([]rune(reply)), Truncate(SingleLine(reply), 200))
	for _, chunk := range SplitText(reply, chat.SplitLimit()) {
		if err := message.Reply(ctx, chunk); err != nil {
			options.Log("发送回复失败：%v", err)
			return
		}
	}
}

// Command 是用户在聊天里输入的控制命令。
type Command struct {
	// Name 是命令名（"reset" / "help"）；空串表示普通聊天
	Name string
}

// ParseCommand 识别聊天控制命令：斜杠命令与直白说法都认，普通聊天不受影响
// （避免用户以为换了话题其实还在老会话里）。
func ParseCommand(text string) Command {
	trimmed := strings.TrimSpace(text)
	switch strings.ToLower(trimmed) {
	case "/new", "/reset", "/clear", "/restart", "重新开始", "新会话", "/新会话", "重置会话":
		return Command{Name: "reset"}
	case "/help", "帮助", "/帮助":
		return Command{Name: "help"}
	}
	return Command{}
}

// HandleCommand 处理控制命令，返回给用户的回复文本。
func HandleCommand(options Options, conversationID string, parsed Command) string {
	switch parsed.Name {
	case "reset":
		// 用户明确要求重新开始：只丢会话映射，工作目录保留（用户的文件与历史都还在）
		if err := options.Conversations.Reset(conversationID); err != nil {
			options.Log("重置会话失败（%s）：%v", conversationID, err)
		}
		workspace, _ := options.Conversations.WorkspaceDir(conversationID)
		options.Log("会话重置（%s）：用户要求重新开始，下一条消息新建 claude 会话", conversationID)
		return "已开始新会话：下一条消息会新建 claude 会话（工作目录保留：" + workspace + "）"
	case "help":
		return HelpText()
	}
	return ""
}

// HelpText 是 /help 的回复：可用命令一览。
func HelpText() string {
	return "可用命令：\n" +
		"/new 重新开始（丢弃当前 claude 会话，工作目录保留）\n" +
		"/help 显示本帮助\n" +
		"专项任务（评审/编程/写作/运维）直接说需求，主 agent 会委派子代理处理。"
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

// AllowedByWhitelist 是三个聊天平台逐字相同的准入算法："*" 通配 → 白名单 →
// 全部拒绝。denyReason 是未配置白名单时的拒绝原因（各平台措辞不同，各自传入）。
//
// 从三份重复实现收敛而来——它与平台无关；weixin 的「登录者兜底」那层不在
// 这里，由 weixin 在调用本函数前自行处理。
func AllowedByWhitelist(adminUsers []string, user, denyReason string) (bool, string) {
	if strings.TrimSpace(user) == "" {
		return false, "用户标识为空"
	}
	for _, admin := range adminUsers {
		if admin == "*" {
			return true, ""
		}
	}
	if len(adminUsers) > 0 {
		for _, admin := range adminUsers {
			if admin == user {
				return true, ""
			}
		}
		return false, "白名单外用户"
	}
	return false, denyReason
}

// CtxDone 报告 ctx 是否已结束。
func CtxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// SleepCtx 睡一段时间，ctx 结束则提前返回。
func SleepCtx(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Truncate 按字符截断（日志用）。
func Truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// SingleLine 把多行文本压成一行（日志一行一条，便于 grep）。
func SingleLine(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
}

// FirstNonEmpty 返回第一个非空白值。
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
