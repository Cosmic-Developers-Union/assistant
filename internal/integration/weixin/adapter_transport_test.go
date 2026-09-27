package weixin

import (
	"testing"
)

// Transport() 必须等于所属通道的 Name()：会话映射层用这个键把「通道 + 用户」
// 映射到会话实体，返回空串或写死平台名会让多开实例（weixin/<name>）串号，
// 回复也走错 bot。
func TestInboundTransportMatchesChannelName(t *testing.T) {
	channel := NewAdapter(AdapterConfig{
		ClientConfig: Config{BaseURL: "http://127.0.0.1:1", BotToken: "tok"},
	}, nil)
	message := adapterInbound{channel: channel, message: Message{
		FromUserID: "u-transport", MessageType: messageTypeUser,
		ItemList: []MessageItem{{Type: itemTypeText, TextItem: &struct {
			Text string `json:"text"`
		}{Text: "状态"}}},
	}}
	if got, want := message.Transport(), channel.Name(); got != want || got == "" {
		t.Errorf("Transport = %q，通道名 = %q（必须相等且非空）", got, want)
	}
	// User 是准入判定与会话键的输入：取发送者
	if got := message.User(); got != "u-transport" {
		t.Errorf("User = %q", got)
	}
}

// 命名实例保留完整键（weixin/<name>），不退回平台名。
func TestNamedInstanceKeepsFullKey(t *testing.T) {
	channel := NewAdapter(AdapterConfig{
		Name:         "weixin/alt",
		ClientConfig: Config{BaseURL: "http://127.0.0.1:1", BotToken: "tok"},
	}, nil)
	if got := channel.Name(); got != "weixin/alt" {
		t.Errorf("Name = %q, want weixin/alt", got)
	}
	if got := channel.SplitLimit(); got != DefaultWeixinSplitLimit {
		t.Errorf("SplitLimit = %d, want %d", got, DefaultWeixinSplitLimit)
	}
}
