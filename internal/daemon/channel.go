// 通用通道层：把「消息平台」抽象成 Channel，通用桥负责会话映射、agent 解析、
// 并发与切块回复。微信（internal/weixin）、QQ（internal/qq）各自实现协议细节。
package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Inbound 是一条入站消息：通用桥通过它读用户与文本、发回复、控制「正在输入」。
// 平台私有的回复上下文（微信 context_token、QQ 的 msg_id/msg_seq）封装在通道
// 实现里，通用桥只认这组方法。
type Inbound interface {
	// Transport 是通道名（与 Channel.Name() 一致）
	Transport() string
	// User 是通道内用户标识
	User() string
	// Text 是文本内容；非文本消息为空，通用桥会跳过
	Text() string
	// Reply 发送一块回复文本
	Reply(ctx context.Context, text string) error
	// Typing 显示/停止「正在输入」（尽力而为，不支持时无操作）
	Typing(ctx context.Context, on bool)
}

// Channel 是聊天服务端的消息后端：把某个平台的消息接进通用桥。实现负责平台
// 协议（长轮询/网关重连/切块协议），通用桥负责准入、会话映射与并发。
type Channel interface {
	// Name 是通道名，同时是会话映射层的 transport 键（weixin/qq）
	Name() string
	// Allowed 判断用户能否对话（白名单/通配/登录者）；拒绝原因仅用于日志
	Allowed(user string) (allow bool, reason string)
	// Receive 阻塞取下一条入站消息；返回错误时通用桥退避后重试
	Receive(ctx context.Context) (Inbound, error)
	// SplitLimit 是回复切块的 rune 上限（<=0 表示不切）
	SplitLimit() int
}

// starter 是通道可选的启动钩子：RunChannel 进入消费循环前调用，失败不致命
// （如微信的上线通知）。
type starter interface {
	Start(ctx context.Context) error
}

// closer 是通道可选的收尾钩子：RunChannel 退出时以短暂超时调用（如下线通知）。
type closer interface {
	Close(ctx context.Context)
}

// ChannelConfig 是通用桥的配置（每个通道一份；Chat 实例跨通道共享）。
type ChannelConfig struct {
	// Chat 是共享的对话会话管理器
	Chat *Chat
	// DefaultAgent 是该通道的默认 agent 名（空 = ChatConfig.DefaultAgent）
	DefaultAgent string
	// Log 输出
	Log func(string, ...any)
	// Debug 为真时输出路由等诊断细节
	Debug bool
	// Concurrency 是该通道同时处理的对话数（缺省 4）
	Concurrency int
}

// RunChannel 常驻消费一个通道的消息：接收 → (准入) → 稳定 claude 会话 →
// 分块回复。同一会话内消息由 Chat 串行，不同会话并发。
func RunChannel(ctx context.Context, channel Channel, config ChannelConfig) error {
	if channel == nil {
		return fmt.Errorf("通道未初始化")
	}
	if config.Chat == nil {
		return fmt.Errorf("对话会话未初始化")
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	if startable, ok := channel.(starter); ok {
		if err := startable.Start(ctx); err != nil {
			config.Log("通道 %s 上线通知失败（忽略）：%v", channel.Name(), err)
		}
	}
	if closable, ok := channel.(closer); ok {
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			closable.Close(stopCtx)
		}()
	}
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	backoff := time.Second
	for !ctxDone(ctx) {
		message, err := channel.Receive(ctx)
		if err != nil {
			if ctxDone(ctx) {
				break
			}
			config.Log("通道 %s 接收失败：%v（%s 后重试）", channel.Name(), err, backoff)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if message == nil {
			continue
		}
		if allowed, reason := channel.Allowed(message.User()); !allowed {
			config.Log("通道 %s 忽略用户 %s 的消息：%s", channel.Name(), message.User(), reason)
			continue
		}
		text := message.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		config.Log("收到消息（%s/%s）：%s", channel.Name(), message.User(), truncate(singleLine(text), 200))
		semaphore <- struct{}{}
		waitGroup.Go(func() {
			defer func() { <-semaphore }()
			handleInbound(ctx, channel, config, message, text)
		})
	}
	waitGroup.Wait()
	return nil
}

// handleInbound 处理单条入站消息：会话映射 → 控制命令/对话 → 切块回复。
// 会话实体由映射层决定：通道 + 通道内用户标识 → conversation id；换通道或把
// 多个通道绑到同一会话时，只改映射表。
func handleInbound(ctx context.Context, channel Channel, config ChannelConfig, message Inbound, text string) {
	conversationID := message.User()
	if mapped, err := config.Chat.ConversationFor(channel.Name(), message.User()); err != nil {
		config.Log("会话映射失败（%s），退回通道用户 id：%v", message.User(), err)
	} else if mapped != "" {
		conversationID = mapped
	}
	message.Typing(ctx, true)
	defer message.Typing(ctx, false)

	var reply string
	if parsed := parseCommand(text); parsed.name != "" {
		reply = handleChatCommand(config, conversationID, parsed)
	} else {
		var err error
		reply, err = config.Chat.Handle(ctx, conversationID, Turn{
			Transport: channel.Name(),
			Text:      text,
			Agent:     config.DefaultAgent,
		})
		if err != nil {
			config.Log("对话失败（%s）：%v", conversationID, err)
			reply = "处理失败：" + err.Error()
		}
	}
	if strings.TrimSpace(reply) == "" {
		reply = "（无回复）"
	}
	config.Log("回复（%s，%d 字）：%s", conversationID, len([]rune(reply)), truncate(singleLine(reply), 200))
	for _, chunk := range splitText(reply, channel.SplitLimit()) {
		if err := message.Reply(ctx, chunk); err != nil {
			config.Log("发送回复失败：%v", err)
			return
		}
	}
}

// command 是用户在聊天里输入的控制命令。
type command struct {
	name string // "reset" / "agent" / "help"；空串表示普通聊天
	arg  string
}

// parseCommand 识别聊天控制命令：斜杠命令与直白说法都认，普通聊天不受影响
// （避免用户以为换了话题其实还在老会话里）。
func parseCommand(text string) command {
	trimmed := strings.TrimSpace(text)
	switch strings.ToLower(trimmed) {
	case "/new", "/reset", "/clear", "/restart", "重新开始", "新会话", "/新会话", "重置会话":
		return command{name: "reset"}
	case "/help", "帮助", "/帮助":
		return command{name: "help"}
	case "/agent", "/agents":
		return command{name: "agent"}
	}
	if name, argument, found := strings.Cut(trimmed, " "); found {
		switch strings.ToLower(name) {
		case "/agent", "/agents":
			return command{name: "agent", arg: strings.TrimSpace(argument)}
		}
	}
	return command{}
}

// restartRequested 判断用户是否明确要求「重新开始」。
func restartRequested(text string) bool {
	return parseCommand(text).name == "reset"
}

// handleChatCommand 处理控制命令，返回给用户的回复文本。
func handleChatCommand(config ChannelConfig, conversationID string, parsed command) string {
	switch parsed.name {
	case "reset":
		// 用户明确要求重新开始：只丢会话映射，工作目录保留（用户的文件与历史都还在）
		if err := config.Chat.Reset(conversationID); err != nil {
			config.Log("重置会话失败（%s）：%v", conversationID, err)
		}
		workspace, _ := config.Chat.WorkspaceDir(conversationID)
		config.Log("会话重置（%s）：用户要求重新开始，下一条消息新建 claude 会话", conversationID)
		return "已开始新会话：下一条消息会新建 claude 会话（工作目录保留：" + workspace + "）"
	case "agent":
		return handleAgentCommand(config, conversationID, parsed.arg)
	case "help":
		return chatHelpText()
	}
	return ""
}

// handleAgentCommand 处理 /agent：无参列出可用 agent 与当前生效；/agent <名>
// 切换并按会话记忆。
func handleAgentCommand(config ChannelConfig, conversationID, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		names := config.Chat.AgentNames()
		if len(names) == 0 {
			return "未配置 agent 池（config.json 的 agents 节），当前使用内置缺省。"
		}
		current := config.Chat.EffectiveAgentName(conversationID, config.DefaultAgent)
		listed := make([]string, 0, len(names))
		for _, candidate := range names {
			if candidate == current {
				listed = append(listed, candidate+"（当前）")
				continue
			}
			listed = append(listed, candidate)
		}
		suffix := "；用 /agent <名> 切换。"
		if current == "" {
			suffix = "；当前使用内置缺省，用 /agent <名> 切换。"
		}
		return "可用 agent：" + strings.Join(listed, "、") + suffix
	}
	if !config.Chat.HasAgent(name) {
		return "没有 agent " + name + "。可用：" + strings.Join(config.Chat.AgentNames(), "、")
	}
	if err := config.Chat.SetAgent(conversationID, name); err != nil {
		config.Log("切换 agent 失败（%s）：%v", conversationID, err)
		return "切换失败：" + err.Error()
	}
	config.Log("会话 %s 切换 agent：%s", conversationID, name)
	return "已切换到 agent " + name + "：下一条消息生效。"
}

// chatHelpText 是 /help 的回复：可用命令一览。
func chatHelpText() string {
	return "可用命令：\n" +
		"/new 重新开始（丢弃当前 claude 会话，工作目录保留）\n" +
		"/agent 查看可用 agent；/agent <名> 切换\n" +
		"/help 显示本帮助"
}

// splitText 把长回复按上限切块（按 rune，尽量在换行处切）。
func splitText(text string, limit int) []string {
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
