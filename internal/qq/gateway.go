package qq

import (
	"context"
	"encoding/json" // 仅用 RawMessage（json/v2 不提供；RawMessage 与 v2 编解码的互操作已验证）
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// 网关 op 码（QQ Bot WebSocket 协议）。
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// IntentGroupAndC2CEvent 订阅群聊 @ 消息与私聊（C2C）消息事件。
const IntentGroupAndC2CEvent = 1 << 25

// 事件类型：本服务端关心的两类消息事件与两个会话事件。
const (
	EventC2CMessage     = "C2C_MESSAGE_CREATE"
	EventGroupAtMessage = "GROUP_AT_MESSAGE_CREATE"
	EventReady          = "READY"
	EventResumed        = "RESUMED"
)

// defaultHeartbeatInterval 是 Hello 未给出间隔时的兜底值。
const defaultHeartbeatInterval = 45 * time.Second

// frame 是网关收发的一帧。
type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  int             `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Gateway 是 QQ Bot 的 WebSocket 网关：Identify 鉴权、心跳保活、断线自动
// Resume/重连；派发事件顺序回调 onEvent（回调阻塞只拖住事件交付，心跳独立运行）。
type Gateway struct {
	client  *Client
	intents int
	log     func(string, ...any)
	debug   bool

	sessionID string
	lastSeq   atomic.Int64
}

// NewGateway 创建网关。intents 是事件订阅位（如 IntentGroupAndC2CEvent）。
func NewGateway(client *Client, intents int) *Gateway {
	return &Gateway{client: client, intents: intents, log: client.log, debug: client.config.Debug}
}

// Run 阻塞运行网关直到 ctx 取消：连接失败按指数退避重试（1s→60s）；服务端
// 要求重连（op 7）或会话失效（op 9）时立即重连。Resume 需要的 session 与 seq
// 在 Gateway 内维护，跨重连保留。凭据/配置类致命错误（*FatalError）不重试，
// 直接返回让通道层停下——修正配置前重试只会刷屏。
func (g *Gateway) Run(ctx context.Context, onEvent func(eventType string, payload json.RawMessage)) error {
	backoff := time.Second
	for !ctxDone(ctx) {
		err := g.connect(ctx, onEvent)
		if ctxDone(ctx) {
			return nil
		}
		if err != nil {
			if _, fatal := errors.AsType[*FatalError](err); fatal {
				g.log("网关遇到不可恢复错误，停止重试（修正配置后重启 assistant）：%v", err)
				return err
			}
			g.log("网关断开：%v（%s 后重连）", err, backoff)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 60*time.Second)
			continue
		}
		// 干净重连（op 7/op 9）：立即重试，退避归零
		backoff = time.Second
	}
	return nil
}

// connect 建立一轮连接：拨号 → Hello → Identify/Resume → 心跳 + 派发循环。
// 返回 nil 表示服务端要求干净重连；返回 err 表示连接异常死亡。
func (g *Gateway) connect(ctx context.Context, onEvent func(string, json.RawMessage)) error {
	gatewayURL, err := g.client.GetGateway(ctx)
	if err != nil {
		return fmt.Errorf("获取网关地址: %w", err)
	}
	if g.debug {
		g.log("连接网关 %s", gatewayURL)
	}
	conn, _, err := websocket.Dial(ctx, gatewayURL, &websocket.DialOptions{HTTPClient: g.client.HTTPClient()})
	if err != nil {
		return fmt.Errorf("连接网关: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	writeFrame := func(f frame) error {
		data, err := jsonv2.Marshal(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(loopCtx, websocket.MessageText, data)
	}

	// Hello 是服务器建连后先发的唯一一帧，带心跳间隔
	hello, err := readFrame(loopCtx, conn, defaultHeartbeatInterval*2)
	if err != nil {
		return fmt.Errorf("等待 Hello: %w", err)
	}
	if hello.Op != opHello {
		return fmt.Errorf("首帧不是 Hello（op=%d）", hello.Op)
	}
	interval := decodeHeartbeatInterval(hello.D)

	// 新会话走 Identify，断线且手里有会话时走 Resume（服务端补发漏掉的事件）
	token, err := g.client.tokens.get(ctx, false)
	if err != nil {
		return err
	}
	if resume := g.sessionID != "" && g.lastSeq.Load() > 0; resume {
		payload, _ := jsonv2.Marshal(map[string]any{
			"token": "QQBot " + token, "session_id": g.sessionID, "seq": g.lastSeq.Load(),
		})
		if err := writeFrame(frame{Op: opResume, D: payload}); err != nil {
			return fmt.Errorf("发送 Resume: %w", err)
		}
		g.log("网关续接会话 %s（seq %d）", shortSessionID(g.sessionID), g.lastSeq.Load())
	} else {
		payload, _ := jsonv2.Marshal(map[string]any{
			"token": "QQBot " + token, "intents": g.intents, "shard": []int{0, 1},
		})
		if err := writeFrame(frame{Op: opIdentify, D: payload}); err != nil {
			return fmt.Errorf("发送 Identify: %w", err)
		}
		g.log("网关鉴权中（intents %d）", g.intents)
	}

	// 心跳：按服务器间隔发 op 1（带最新 seq）
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				var payload = json.RawMessage("null")
				if seq := g.lastSeq.Load(); seq > 0 {
					payload, _ = jsonv2.Marshal(seq)
				}
				if err := writeFrame(frame{Op: opHeartbeat, D: payload}); err != nil {
					if g.debug {
						g.log("心跳发送失败：%v", err)
					}
					return
				}
				if g.debug {
					g.log("心跳 seq=%d", g.lastSeq.Load())
				}
			}
		}
	}()

	// 读循环：2×interval 收不到任何帧（连心跳 ACK 都没有）按连接假死处理
	for {
		f, err := readFrame(loopCtx, conn, interval*2)
		if err != nil {
			return err
		}
		switch f.Op {
		case opDispatch:
			if f.S > 0 && int64(f.S) > g.lastSeq.Load() {
				g.lastSeq.Store(int64(f.S))
			}
			switch f.T {
			case EventReady:
				var ready struct {
					SessionID string `json:"session_id"`
				}
				_ = jsonv2.Unmarshal(f.D, &ready)
				g.sessionID = ready.SessionID
				g.log("网关就绪（会话 %s）", shortSessionID(ready.SessionID))
			case EventResumed:
				g.log("网关续接成功")
			default:
				onEvent(f.T, f.D)
			}
		case opHeartbeatACK:
			// 保活确认（读超时兜底已覆盖假死连接）
		case opReconnect:
			g.log("服务端要求重连（op 7）")
			return nil
		case opInvalidSession:
			g.log("会话失效（op 9），下次重连重新 Identify")
			g.sessionID = ""
			g.lastSeq.Store(0)
			return nil
		default:
			if g.debug {
				g.log("未处理的 op %d", f.Op)
			}
		}
	}
}

// readFrame 读一帧，超过 timeout 没有任何字节按失败处理。
func readFrame(ctx context.Context, conn *websocket.Conn, timeout time.Duration) (frame, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return frame{}, err
	}
	var f frame
	if err := jsonv2.Unmarshal(data, &f); err != nil {
		return frame{}, fmt.Errorf("解析帧: %w", err)
	}
	return f, nil
}

// decodeHeartbeatInterval 解析 Hello 的心跳间隔（毫秒）。
func decodeHeartbeatInterval(payload json.RawMessage) time.Duration {
	var hello struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	_ = jsonv2.Unmarshal(payload, &hello)
	if hello.HeartbeatInterval <= 0 {
		return defaultHeartbeatInterval
	}
	return time.Duration(hello.HeartbeatInterval) * time.Millisecond
}

func shortSessionID(sessionID string) string {
	if len(sessionID) > 8 {
		return sessionID[:8] + "…"
	}
	return sessionID
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
