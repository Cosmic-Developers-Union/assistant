package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/integration"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/qq"
	qqtestsupport "github.com/Cosmic-Developers-Union/assistant/internal/integration/qq/qqtestsupport"
	"github.com/coder/websocket"
)

// newQQAdapterForTest 起一个 QQ 通道 + fake API + RunChat（返回的 cancel
// 由调用方在断言后触发）。
func newQQAdapterForTest(t *testing.T, config qq.AdapterConfig) (*qqtestsupport.Fake, *qq.Adapter, context.CancelFunc, chan error) {
	t.Helper()
	fake := qqtestsupport.New(t)
	config.APIBaseURL = fake.URL()
	config.TokenURL = fake.URL() + "/token"
	config.HTTPClient = fake.Client()
	channel := qq.NewAdapter(config, "", false, t.Logf)
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		Claude:     runnerSuccess("好的"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- integration.RunChat(ctx, channel, integration.Options{Conversations: IntegrateConversations(chat), Log: t.Logf})
	}()
	return fake, channel, cancel, done
}

// 私聊：白名单用户的消息 → claude 回复以被动消息发回（带 msg_id、msg_seq=1）。
func TestQQAdapterRepliesWithMsgID(t *testing.T) {
	fake, _, cancel, done := newQQAdapterForTest(t, qq.AdapterConfig{
		AppID: "app-1", AppSecret: "sec-1", AdminUsers: []string{"u1"},
	})
	fake.SetWS(func(t *testing.T, conn *websocket.Conn) {
		qqtestsupport.WriteFrame(t, conn, `{"op":10,"d":{"heartbeat_interval":80}}`)
		qqtestsupport.ReadFrame(t, conn, 2*time.Second)
		qqtestsupport.WriteFrame(t, conn, `{"op":0,"s":1,"t":"READY","d":{"session_id":"s","user":{"id":"bot"}}}`)
		qqtestsupport.WriteFrame(t, conn, `{"op":0,"s":2,"t":"C2C_MESSAGE_CREATE","d":{"id":"m-1","content":"你好","author":{"user_openid":"u1"}}}`)
		for {
			if next, ok := qqtestsupport.ReadFrame(t, conn, 2*time.Second); !ok {
				return
			} else if strings.Contains(string(next), `"op":1`) {
				qqtestsupport.WriteFrame(t, conn, `{"op":11}`)
			}
		}
	})
	sent := fake.WaitSent(t, 1)
	cancel()
	<-done
	if sent[0]["content"] != "好的" || sent[0]["msg_id"] != "m-1" || sent[0]["msg_seq"] != float64(1) {
		t.Errorf("回复 payload = %+v", sent[0])
	}
}

// 群聊：@ 消息（残留提及前缀被清理）→ 回到对应群，白名单按成员 openid 判定。
func TestQQAdapterGroupReply(t *testing.T) {
	fake, _, cancel, done := newQQAdapterForTest(t, qq.AdapterConfig{
		AppID: "app-1", AppSecret: "sec-1", AdminUsers: []string{"*"},
	})
	fake.SetWS(func(t *testing.T, conn *websocket.Conn) {
		qqtestsupport.WriteFrame(t, conn, `{"op":10,"d":{"heartbeat_interval":80}}`)
		qqtestsupport.ReadFrame(t, conn, 2*time.Second)
		qqtestsupport.WriteFrame(t, conn, `{"op":0,"s":1,"t":"READY","d":{"session_id":"s","user":{"id":"bot"}}}`)
		qqtestsupport.WriteFrame(t, conn, `{"op":0,"s":2,"t":"GROUP_AT_MESSAGE_CREATE","d":{"id":"g-1","content":"<@!PKIXXBOT> 查下状态","group_openid":"GG1","author":{"member_openid":"m1"}}}`)
		for {
			if next, ok := qqtestsupport.ReadFrame(t, conn, 2*time.Second); !ok {
				return
			} else if strings.Contains(string(next), `"op":1`) {
				qqtestsupport.WriteFrame(t, conn, `{"op":11}`)
			}
		}
	})
	sent := fake.WaitSent(t, 1)
	cancel()
	<-done
	if sent[0]["msg_id"] != "g-1" {
		t.Errorf("回复 payload = %+v", sent[0])
	}
	// claude 收到的应是清理后的文本：用回复内容无法验证入参，这里验证切块路径
	// 走通即可；文本清理逻辑单测见 TestCleanQQContent。
}

// 准入：未配置白名单时全部拒绝（不回复也不建会话）。
func TestQQAdapterDeniesWithoutWhitelist(t *testing.T) {
	fake, channel, cancel, done := newQQAdapterForTest(t, qq.AdapterConfig{
		AppID: "app-1", AppSecret: "sec-1",
	})
	if allowed, reason := channel.Allowed("anyone"); allowed || reason == "" {
		t.Errorf("未配置白名单应全拒：allowed=%v reason=%q", allowed, reason)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if sent := fake.SentSnapshot(); len(sent) != 0 {
		t.Errorf("不应有发送：%v", sent)
	}
}

// 文本清理：残留的 @ 提及前缀（<@!bot>、<@bot>、@机器人）被剥掉，正文保留。
