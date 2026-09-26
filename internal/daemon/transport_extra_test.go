package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/qq"
	"github.com/Cosmic-Developers-Union/assistant/internal/weixin"
)

// TestInboundTransportMatchesChannelName 钉住三个通道入站消息的 Transport 契约：
// 它必须等于所属通道的 Name()——会话映射层用这个键把「通道 + 用户」映射到会话
// 实体，返回空串或写死平台名会让多开实例（telegram/<name>）串号，回复也走错
// bot。这里同时覆盖此前零覆盖的三个一行访问器。
func TestInboundTransportMatchesChannelName(t *testing.T) {
	weixinChannel := NewWeixinChannel(WeixinChannelConfig{
		Weixin: weixin.Config{BaseURL: "http://127.0.0.1:1", BotToken: "tok"},
	}, nil)
	weixinMessage := weixinInbound{channel: weixinChannel, message: weixin.Message{
		FromUserID: "u-transport", MessageType: messageTypeUser,
		ItemList: []weixin.MessageItem{{Type: 1, TextItem: &struct {
			Text string `json:"text"`
		}{Text: "状态"}}},
	}}
	if got, want := weixinMessage.Transport(), weixinChannel.Name(); got != want || got == "" {
		t.Errorf("weixin Transport = %q，通道名 = %q（必须相等且非空）", got, want)
	}

	qqChannel := newQQChannelWithClient(QQChannelConfig{AppID: "app", AppSecret: "sec", AdminUsers: []string{"*"}},
		qq.NewClient(qq.Config{AppID: "app", AppSecret: "sec", APIBaseURL: "http://127.0.0.1:1"}), "qq/alt", false, nil)
	qqMessage := qqInbound{channel: qqChannel, userOpenID: "u-transport", msgID: "m-1", text: "状态"}
	if got, want := qqMessage.Transport(), qqChannel.Name(); got != want || got != "qq/alt" {
		t.Errorf("qq Transport = %q，通道名 = %q（命名实例应保留完整键）", got, want)
	}

	telegramChannel := NewTelegramChannel(TelegramChannelConfig{Name: "telegram/alt", BotToken: "tok"}, nil)
	telegramMessage := telegramInbound{channel: telegramChannel, userID: "u-transport", chatID: "u-transport"}
	if got, want := telegramMessage.Transport(), telegramChannel.Name(); got != want || got != "telegram/alt" {
		t.Errorf("telegram Transport = %q，通道名 = %q（命名实例应保留完整键）", got, want)
	}

	// User() 是准入判定与会话键的输入：weixin 取发送者、qq 取 openid、telegram 取数字 id
	if got := weixinMessage.User(); got != "u-transport" {
		t.Errorf("weixin User = %q", got)
	}
	if got := qqMessage.User(); got != "u-transport" {
		t.Errorf("qq User = %q", got)
	}
	if got := telegramMessage.User(); got != "u-transport" {
		t.Errorf("telegram User = %q", got)
	}
}

// TestSleepCtxWaitsOutFullTimer 钉住 sleepCtx 的正常路径：context 未取消时必须
// 真的等满时长再返回（微信 ret=-14 的重试节流靠它，提前返回会变成热循环打爆
// 平台限频）。
func TestSleepCtxWaitsOutFullTimer(t *testing.T) {
	start := time.Now()
	sleepCtx(t.Context(), 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("应等满 50ms，实际只等了 %v", elapsed)
	}
}

// TestSleepCtxReturnsImmediatelyOnCancel 钉住 sleepCtx 的取消路径：context 取消
// 后必须立刻返回而不是继续睡——daemon 退出时通道协程要能马上收尾，否则最长会
// 拖住一小时的退避睡眠（微信 ret=-14 分支）。
func TestSleepCtxReturnsImmediatelyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Hour)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("已取消的 context 应立即返回，实际等了 %v", elapsed)
	}
}
