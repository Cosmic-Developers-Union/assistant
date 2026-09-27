// daemon 与 integration 的接缝：把 Chat 暴露成 integration.Conversations，
// 供通用运行循环（internal/integration.RunChat）驱动各平台集成。
//
// 存在这一步是为了让 internal/integration/<platform> 只依赖通用层、**不依赖
// daemon**——否则各平台包仍被拖着，集成无法独立测试。
package daemon

import (
	"context"

	"github.com/Cosmic-Developers-Union/assistant/internal/integration"
)

// 编译期断言：包装层确实满足通用层的对话面。
var _ integration.Conversations = chatConversations{}

// IntegrateConversations 把 Chat 包装成通用层要的对话面：Turn 类型在此转换
// （Chat.Handle 收 daemon 自己的 Turn，通用层收 integration.Turn，语义一致）。
func IntegrateConversations(chat *Chat) integration.Conversations {
	return chatConversations{chat: chat}
}

type chatConversations struct {
	chat *Chat
}

func (c chatConversations) ConversationFor(transport, user string) (string, error) {
	return c.chat.ConversationFor(transport, user)
}

func (c chatConversations) Handle(ctx context.Context, conversationID string, turn integration.Turn) (string, error) {
	return c.chat.Handle(ctx, conversationID, Turn{Transport: turn.Transport, Text: turn.Text})
}

func (c chatConversations) Reset(conversationID string) error {
	return c.chat.Reset(conversationID)
}

func (c chatConversations) WorkspaceDir(conversationID string) (string, error) {
	return c.chat.WorkspaceDir(conversationID)
}
