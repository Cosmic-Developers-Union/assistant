package qq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	qqtestsupport "github.com/Cosmic-Developers-Union/assistant/internal/integration/qq/qqtestsupport"
)

// TestNewAdapterDefaultsAndAccessors 钉住公开构造 NewAdapter 的装配缺省：
// 空 key 归一化成 "qq"、SplitLimit<=0 回落到平台上限 1000、用户标识为空则拒绝。
// 这条路径此前零覆盖（既有测试都走 newAdapterWithClient），而 daemon 装配真实
// 通道用的正是它——缺省错了会让通道注册键对不上配置里的名字。
func TestNewAdapterDefaultsAndAccessors(t *testing.T) {
	channel := NewAdapter(AdapterConfig{AppID: "app", AppSecret: "sec", APIBaseURL: "http://127.0.0.1:1"}, "", false, nil)
	if channel.Name() != "qq" {
		t.Errorf("空 key 应归一化成 qq：%q", channel.Name())
	}
	if got := channel.SplitLimit(); got != DefaultQQSplitLimit {
		t.Errorf("SplitLimit = %d，want %d", got, DefaultQQSplitLimit)
	}
	if allowed, reason := channel.Allowed("  "); allowed || reason != "用户标识为空" {
		t.Errorf("空用户标识应拒绝：%v %q", allowed, reason)
	}

	named := NewAdapter(AdapterConfig{SplitLimit: 50, AdminUsers: []string{"*"}}, " qq/alt ", false, nil)
	if named.Name() != "qq/alt" {
		t.Errorf("自定义 key 应去空白后原样：%q", named.Name())
	}
	if got := named.SplitLimit(); got != 50 {
		t.Errorf("显式 SplitLimit 应生效：%d", got)
	}
	if allowed, reason := named.Allowed("任何人"); !allowed || reason != "" {
		t.Errorf("通配白名单应放行：%v %q", allowed, reason)
	}
}

// TestQQAllowedWhitelistBranches 钉住 Allowed 的白名单两条分支：命中放行、未命中
// 拒绝，以及完全未配置时的文案（必须点出 admin_users 这个配置键，否则操作者
// 不知道该去哪加人）。
func TestQQAllowedWhitelistBranches(t *testing.T) {
	channel := NewAdapter(AdapterConfig{AdminUsers: []string{"u1", "u2"}}, "", false, nil)
	if allowed, reason := channel.Allowed("u2"); !allowed || reason != "" {
		t.Errorf("白名单命中应放行：%v %q", allowed, reason)
	}
	if allowed, reason := channel.Allowed("u3"); allowed || reason != "白名单外用户" {
		t.Errorf("白名单外应拒绝：%v %q", allowed, reason)
	}

	empty := NewAdapter(AdapterConfig{}, "", false, nil)
	if allowed, reason := empty.Allowed("u1"); allowed || !strings.Contains(reason, "admin_users") {
		t.Errorf("未配置白名单应拒绝并提示配置键：%v %q", allowed, reason)
	}
}

// TestAdapterStartFatalErrorPropagates 钉住 Start 的致命路径：网关因不可恢复的
// 错误退出（这里是 token 端点被平台拒绝）时必须记下 fatalErr 并关闭 dead，让
// Receive 把原因交给通用桥。若吞掉错误，通道会永远静默——用户发消息没有任何反应
// 也没有任何日志，最坏的一种故障。
func TestAdapterStartFatalErrorPropagates(t *testing.T) {
	fake := qqtestsupport.New(t)
	// 让 gateway 拿不到凭据：token 端点直接 401（网关重试耗尽后返回错误）
	fake.Server().Config.Handler = deniedTokenHandler()
	client := NewClient(Config{
		AppID: "app", AppSecret: "sec",
		APIBaseURL: fake.URL(), TokenURL: fake.URL() + "/token",
		HTTPClient: fake.Client(), Log: t.Logf,
	})
	channel := newAdapterWithClient(AdapterConfig{}, client, "", false, t.Logf)
	if err := channel.Start(t.Context()); err != nil {
		t.Fatalf("Start 应立刻返回 nil（网关在后台）：%v", err)
	}
	// dead 关闭后 Receive 必须交出 fatalErr（而不是 nil, nil）
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("等待网关致命退出超时")
		default:
		}
		message, err := channel.Receive(t.Context())
		if err != nil {
			if message != nil {
				t.Errorf("致命退出时应返回 nil 消息：%+v", message)
			}
			if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "凭据") &&
				!strings.Contains(err.Error(), "token") {
				t.Errorf("致命原因应点名凭据问题：%v", err)
			}
			return
		}
		t.Fatalf("网关未退出前不应交出消息：%+v", message)
	}
}

// deniedTokenHandler 返回一个所有请求都 401 的处理器（模拟凭据被平台拒绝）。
func deniedTokenHandler() interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
} {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"code":100007,"message":"appid invalid"}`))
	})
}

// TestAdapterReceiveDeadAndContext 钉住 Receive 的两条非队列分支：dead 关闭时
// 交出 fatalErr（网关致命退出的交接点），ctx 取消时交出 ctx.Err()（daemon 停机
// 不能让桥挂死）。两条都不经网络，直接构造通道状态即可。
func TestAdapterReceiveDeadAndContext(t *testing.T) {
	channel := NewAdapter(AdapterConfig{}, "", false, nil)

	// 手工模拟网关致命退出
	channel.fatalErr = errors.New("网关重连耗尽")
	close(channel.dead)
	if _, err := channel.Receive(t.Context()); !errors.Is(err, channel.fatalErr) {
		t.Errorf("dead 关闭应交出 fatalErr：%v", err)
	}

	// ctx 已取消
	other := NewAdapter(AdapterConfig{}, "", false, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := other.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消应交出 ctx.Err()：%v", err)
	}

	// 事件队列有条目时优先交出（dead 未关闭）
	other.events <- &adapterInbound{channel: other, userOpenID: "u", msgID: "m", text: "内容"}
	message, err := other.Receive(t.Context())
	if err != nil || message.Text() != "内容" {
		t.Errorf("队列有消息应先交出：%v %+v", err, message)
	}
}

// TestQQOnEventFiltersAndDelivers 钉住网关回调的过滤四则与投递：不关心的事件
// 类型（好友添加）、坏 JSON（平台协议变更时的日志线索）、缺用户或 msg_id、
// 清理后为空的文本都必须被丢弃（投递空消息会让 claude 收到空提问）；合法消息
// 必须落进队列并按群/私聊取正确的用户标识。
func TestQQOnEventFiltersAndDelivers(t *testing.T) {
	var logs []string
	channel := NewAdapter(AdapterConfig{}, "", false, func(format string, arguments ...any) {
		logs = append(logs, format)
	})
	channel.runCtx = t.Context()

	channel.onEvent("FRIEND_ADD", json.RawMessage(`{"id":"m"}`)) // 不关心的事件类型
	channel.onEvent(EventC2CMessage, json.RawMessage(`{不是 JSON`))
	channel.onEvent(EventC2CMessage, json.RawMessage(`{"content":"没有用户"}`))
	channel.onEvent(EventC2CMessage, json.RawMessage(`{"id":"m-1","content":"   ","author":{"user_openid":"u1"}}`))
	if len(channel.events) != 0 {
		t.Fatalf("四条都应被丢弃，实际 %d 条", len(channel.events))
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "解析 QQ 消息事件失败") || !strings.Contains(joined, "缺少用户或 msg_id") {
		t.Errorf("丢弃原因必须进日志：%v", logs)
	}

	// 私聊：user_openid 优先
	channel.onEvent(EventC2CMessage, json.RawMessage(
		`{"id":"m-1","content":" 你好 ","author":{"user_openid":"u1","id":"ignored"}}`))
	// 群聊：成员 openid 优先，且残留提及前缀被清理
	channel.onEvent(EventGroupAtMessage, json.RawMessage(
		`{"id":"m-2","content":"<@!BOT> 群里的问题","group_openid":"G1","author":{"member_openid":"mem1"}}`))
	if len(channel.events) != 2 {
		t.Fatalf("两条合法消息都应投递：%d", len(channel.events))
	}
	private := (<-channel.events).(*adapterInbound)
	if private.User() != "u1" || private.Text() != "你好" || private.groupOpenID != "" {
		t.Errorf("私聊归一化 = %+v", private)
	}
	group := (<-channel.events).(*adapterInbound)
	if group.User() != "mem1" || group.Text() != "群里的问题" || group.groupOpenID != "G1" {
		t.Errorf("群聊归一化 = %+v", group)
	}
	if group.Transport() != "qq" {
		t.Errorf("Transport = %q", group.Transport())
	}
}

// TestQQOnEventUnblocksOnRunContextDone 钉住队列满时的背压出口：events 打满后
// onEvent 会阻塞，必须靠 runCtx 取消解围——否则 daemon 停机时网关回调协程永久
// 卡住，进程关不掉。这里把队列灌满再取消上下文，onEvent 必须立刻返回。
func TestQQOnEventUnblocksOnRunContextDone(t *testing.T) {
	channel := NewAdapter(AdapterConfig{}, "", false, nil)
	ctx, cancel := context.WithCancel(t.Context())
	channel.runCtx = ctx
	payload := json.RawMessage(`{"id":"m","content":"x","author":{"user_openid":"u"}}`)
	for range qqEventQueueSize {
		channel.onEvent(EventC2CMessage, payload)
	}
	if len(channel.events) != qqEventQueueSize {
		t.Fatalf("队列应恰好打满：%d", len(channel.events))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		channel.onEvent(EventC2CMessage, payload) // 队列满 → 只能等 runCtx
	}()
	select {
	case <-done:
		t.Fatal("队列满时不应立刻返回（应先经历一次阻塞）")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runCtx 取消后 onEvent 必须返回")
	}
	if len(channel.events) != qqEventQueueSize {
		t.Errorf("满队列不应被顶掉条目：%d", len(channel.events))
	}
}

// TestQQInboundReplySeqAndGroupRouting 钉住 msg_seq 递增与清空边界：同一 msg_id
// 的多次回复序号必须递增（平台按 msg_id+msg_seq 去重，重号会吞掉后续分块），
// 缓存超过上限时整体清空（避免无界增长）——注意清空发生在写入之后，当下这条
// 计数也被一并抹掉，于是清空后的下一条回复又从 1 开始（现状，见断言注释）。
// 群聊回复必须走群接口、私聊走 C2C 接口。
func TestQQInboundReplySeqAndGroupRouting(t *testing.T) {
	fake := qqtestsupport.New(t)
	client := NewClient(Config{
		AppID: "app", AppSecret: "sec",
		APIBaseURL: fake.URL(), TokenURL: fake.URL() + "/token",
		HTTPClient: fake.Client(), Log: t.Logf,
	})
	channel := newAdapterWithClient(AdapterConfig{}, client, "", false, t.Logf)

	private := &adapterInbound{channel: channel, userOpenID: "u1", msgID: "m-1", text: "问"}
	for want := 1; want <= 2; want++ {
		if err := private.Reply(t.Context(), "分块"); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if got := channel.msgSeq["m-1"]; got != want {
			t.Errorf("msg_seq 应递增到 %d，实际 %d", want, got)
		}
	}
	group := &adapterInbound{channel: channel, userOpenID: "mem", groupOpenID: "G1", msgID: "g-1", text: "问"}
	if err := group.Reply(t.Context(), "群里的回复"); err != nil {
		t.Fatalf("群聊 Reply: %v", err)
	}
	sent := fake.WaitSent(t, 3)
	// fake 只记录 /v2/ 的发送（token 请求走 /token，不入账），索引即发送顺序：
	// 私聊 m-1 的两块在前，群聊 g-1 在后。
	// 收信方不在请求体里——SendC2CText/SendGroupText 把 user_openid / group_openid
	// 编进 URL 路径（/v2/users/<id>/messages 与 /v2/groups/<id>/messages），体里
	// 只有 content/msg_type/msg_id/msg_seq。群聊必须打群接口（回到群里），私聊
	// 必须打 C2C 接口（回到发信人），打错接口等于把群里的回复私发给某个人。
	paths := fake.SentPaths()
	if want := []string{
		"/v2/users/u1/messages", "/v2/users/u1/messages", "/v2/groups/G1/messages",
	}; !slices.Equal(paths, want) {
		t.Errorf("回复接口路径 = %v，want %v", paths, want)
	}
	if sent[2]["msg_id"] != "g-1" || sent[2]["msg_seq"] != float64(1) {
		t.Errorf("群聊回复 payload = %+v", sent[2])
	}
	if sent[0]["msg_id"] != "m-1" || sent[0]["msg_seq"] != float64(1) ||
		sent[0]["content"] != "分块" {
		t.Errorf("私聊第一块 payload = %+v", sent[0])
	}
	if sent[1]["msg_id"] != "m-1" || sent[1]["msg_seq"] != float64(2) {
		t.Errorf("私聊第二块 payload = %+v", sent[1])
	}

	// 缓存超过上限：整体清空。注意 clear 紧跟在写入之后，因此当下这条计数也一并
	// 被抹掉（现状，见 Reply）——清空后的下一条回复又从 1 开始。
	flood := &adapterInbound{channel: channel, userOpenID: "u", msgID: "触发清空", text: "x"}
	channel.seqMu.Lock()
	channel.msgSeq[flood.msgID] = 1 // 自身先占一条
	for index := range qqMsgSeqCacheLimit + 1 - 3 {
		channel.msgSeq["f-"+strings.Repeat("y", index)] = 1
	}
	if len(channel.msgSeq) != qqMsgSeqCacheLimit+1 {
		t.Fatalf("准备阶段应恰好超过上限：%d", len(channel.msgSeq))
	}
	channel.seqMu.Unlock()
	if err := flood.Reply(t.Context(), "触发"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(channel.msgSeq) != 0 {
		t.Errorf("超过上限应整体清空：%d", len(channel.msgSeq))
	}
	// 清空后同一个 msg_id 再回复：序号回到 1（当下这条的计数已被 clear 抹掉）
	if err := flood.Reply(t.Context(), "清空后"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := channel.msgSeq["触发清空"]; got != 1 {
		t.Errorf("清空后序号应重置为 1，实际 %d", got)
	}
}

// TestQQInboundTypingIsNoop 钉住 Typing 的空实现：QQ 开放平台没有「正在输入」
// 协议，必须静默无事（发不出请求也不能 panic 或报错）。
func TestQQInboundTypingIsNoop(t *testing.T) {
	channel := NewAdapter(AdapterConfig{}, "", false, nil)
	inbound := &adapterInbound{channel: channel, userOpenID: "u", msgID: "m"}
	inbound.Typing(t.Context(), true)
	inbound.Typing(t.Context(), false)
	if inbound.User() != "u" || inbound.Text() != "" {
		t.Errorf("Typing 不应改动入站消息：%+v", inbound)
	}
}

// TestCleanQQContentUnterminatedPrefixes 钉住提及清理的两条提前返回与循环上限：
// `@` 后面没有空格时必须原样返回（不能把剩余文本整段吞掉——用户打「@所有人」这
// 类自然文本时不该丢内容）；`<@` 未闭合时 Cut 同样找不到 `>`，也原样返回。前缀
// 剥离固定只跑两轮：第三层提及会残留在文本里（现状，平台通常只带一个）。
func TestCleanQQContentUnterminatedPrefixes(t *testing.T) {
	cases := map[string]string{
		"<@没有闭合尖括号":   "<@没有闭合尖括号", // 找不到 `>` 原样返回
		"@没有空格":       "@没有空格",     // 找不到空格原样返回
		"<@A> 内容":     "内容",
		"<@!BOT> 内容":  "内容",
		"@bot @me 问题": "问题",    // 两轮循环：两个 @ 前缀都被剥掉
		"@a @b @c 三层": "@c 三层", // 只剥两轮，第三层残留（现状）
		// TrimSpace 去掉首尾空白后再剥前缀
		"  <@!BOT> 问题 ": "问题",
		"纯文本":           "纯文本",
	}
	for input, want := range cases {
		if got := cleanQQContent(input); got != want {
			t.Errorf("cleanQQContent(%q) = %q，want %q", input, got, want)
		}
	}
}

var _ = sync.Mutex{}
