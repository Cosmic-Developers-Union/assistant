package aigateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, text string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("解析测试体: %v", err)
	}
	return body
}

// 抽取优先级：显式头 > metadata.user_id > 内容派生。
func TestSessionResolvePriority(t *testing.T) {
	resolver := &SessionResolver{Secret: []byte("secret")}
	header := http.Header{}
	header.Set("x-opencode-session", "header-session")
	body := decodeBody(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"metadata":{"user_id":"{\"session_id\":\"meta-session\",\"device_id\":\"d1\"}"}}`)
	session := resolver.Resolve(header, body, "key-a")
	if session.ID != "header-session" || session.Source != "header:x-opencode-session" {
		t.Errorf("session = %+v", session)
	}

	// 头缺失时用 metadata
	session = resolver.Resolve(http.Header{}, body, "key-a")
	if session.ID != "meta-session" || session.Source != "metadata.user_id" {
		t.Errorf("session = %+v", session)
	}

	// metadata 为纯字符串（老式）也识别
	plain := decodeBody(t, `{"metadata":{"user_id":"plain-session"}}`)
	if session := resolver.Resolve(http.Header{}, plain, "key-a"); session.ID != "plain-session" {
		t.Errorf("session = %+v", session)
	}

	// 都没有时按内容派生
	noMeta := decodeBody(t, `{"model":"m","messages":[{"role":"user","content":"first"}]}`)
	derived := resolver.Resolve(http.Header{}, noMeta, "key-a")
	if derived.Source != "derived:content" || derived.ID == "" {
		t.Errorf("session = %+v", derived)
	}
}

// 内容派生必须稳定：多轮对话（历史变长、后续消息不同）仍落同一键；首条消息、
// 模型或客户端变化时换键。
func TestSessionDeriveStable(t *testing.T) {
	resolver := &SessionResolver{Secret: []byte("secret")}
	first := decodeBody(t, `{"model":"m","system":"sys","messages":[
		{"role":"user","content":"第一轮问题"},
		{"role":"assistant","content":"回答"},
		{"role":"user","content":"第二轮追问"}]}`)
	second := decodeBody(t, `{"model":"m","system":"sys","messages":[
		{"role":"user","content":"第一轮问题"},
		{"role":"assistant","content":"回答"},
		{"role":"user","content":"第二轮追问"},
		{"role":"assistant","content":"再答"},
		{"role":"user","content":"第三轮"}]}`)
	one := resolver.Resolve(http.Header{}, first, "key-a")
	two := resolver.Resolve(http.Header{}, second, "key-a")
	if one.ID != two.ID {
		t.Errorf("多轮应稳定：%q vs %q", one.ID, two.ID)
	}
	other := decodeBody(t, `{"model":"m","system":"sys","messages":[{"role":"user","content":"换了问题"}]}`)
	if resolver.Resolve(http.Header{}, other, "key-a").ID == one.ID {
		t.Error("首条消息变化应换键")
	}
	otherModel := decodeBody(t, `{"model":"m2","system":"sys","messages":[{"role":"user","content":"第一轮问题"}]}`)
	if resolver.Resolve(http.Header{}, otherModel, "key-a").ID == one.ID {
		t.Error("模型变化应换键")
	}
	if resolver.Resolve(http.Header{}, first, "key-b").ID == one.ID {
		t.Error("客户端变化应换键")
	}
	// 内容块数组形式的 system/messages 同样参与派生
	blocks := decodeBody(t, `{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[
		{"role":"user","content":[{"type":"text","text":"第一轮问题"}]}]}`)
	if resolver.Resolve(http.Header{}, blocks, "key-a").ID != one.ID {
		t.Error("内容块与纯文本应派生出同一键")
	}
}

// 无 Secret 时仍确定性（退化 SHA-256）。
func TestSessionDeriveWithoutSecret(t *testing.T) {
	resolver := &SessionResolver{}
	body := decodeBody(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	first := resolver.Resolve(http.Header{}, body, "k")
	second := resolver.Resolve(http.Header{}, body, "k")
	if first.ID != second.ID || first.ID == "" {
		t.Errorf("无密钥派生应稳定：%q vs %q", first.ID, second.ID)
	}
}

// 超长/含控制字符的显式标识规范化为 UUID。
func TestSessionNormalizeOversized(t *testing.T) {
	resolver := &SessionResolver{}
	header := http.Header{}
	header.Set("x-session-id", strings.Repeat("a", 300))
	session := resolver.Resolve(header, nil, "k")
	if len(session.ID) != 36 || session.ID[14] != '5' {
		t.Errorf("应规范化为 UUID：%q", session.ID)
	}
}

// Body：未改写时逐字节返回原始体；写入时合并 metadata.user_id（保留既有键）。
func TestBodyPassthroughAndMetadataMerge(t *testing.T) {
	raw := []byte(`{"model":"m","metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"old\"}"}}`)
	body := NewBody(raw)
	if body.Mutated() {
		t.Fatal("初始不应标记改写")
	}
	if encoded, _ := body.Encoded(); string(encoded) != string(raw) {
		t.Error("未改写应返回原始字节")
	}
	body.MetadataSession("new-session")
	encoded, err := body.Encoded()
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	metadata, _ := document["metadata"].(map[string]any)
	var userID map[string]any
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &userID); err != nil {
		t.Fatal(err)
	}
	if userID["session_id"] != "new-session" || userID["device_id"] != "d1" {
		t.Errorf("metadata.user_id = %+v", userID)
	}
	// Set 顶层字段
	other := NewBody([]byte(`{"model":"m"}`))
	other.Set("prompt_cache_key", "s1")
	encoded, _ = other.Encoded()
	if !strings.Contains(string(encoded), `"prompt_cache_key":"s1"`) {
		t.Errorf("encoded = %s", encoded)
	}
}
