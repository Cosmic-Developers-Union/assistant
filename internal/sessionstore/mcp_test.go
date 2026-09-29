package sessionstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
)

// seedIndex 造一个已归档一条 chat 会话的本地索引。
func seedIndex(t *testing.T) *sessionindex.Store {
	t.Helper()
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatalf("打开索引失败：%v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	record := sessionindex.Record{
		Key:          sessionindex.Key{Host: "node-1", Project: "-home-ge-chat-chat-1a2b", Session: "s-chat"},
		Source:       "chat",
		Conversation: "c-1a2b3c4d",
		Transport:    "weixin",
		Title:        "chat-1a2b",
		UpdatedAt:    "2026-09-29T10:00:00Z",
	}
	if err := index.Upsert(record, extractAll(sampleLines())); err != nil {
		t.Fatalf("写入索引失败：%v", err)
	}
	return index
}

// MCP 工具：session_search / session_list / session_read / conversation_list 四个工具
// 直接查本地索引，返回可直接给 agent 读的文本。
func TestRunMCPTools(t *testing.T) {
	index := seedIndex(t)

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"session_search","arguments":{"query":"待评审"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"session_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"session_read","arguments":{"host":"node-1","project":"-home-ge-chat-chat-1a2b","session":"s-chat","limit":3}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"conversation_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"bogus/method"}`,
	}
	var input bytes.Buffer
	for _, line := range requests {
		input.WriteString(line + "\n")
	}
	var output bytes.Buffer
	if err := RunMCP(context.Background(), &input, &output, MCPOptions{Index: index}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}

	responses := map[int]map[string]any{}
	scanner := bufio.NewScanner(&output)
	scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for scanner.Scan() {
		var doc map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &doc); err != nil {
			t.Fatalf("响应不是 JSON：%s", scanner.Text())
		}
		id, _ := doc["id"].(float64)
		responses[int(id)] = doc
	}
	if len(responses) != 8 {
		t.Fatalf("响应条数 = %d（notifications 不该有响应）", len(responses))
	}
	if info, _ := responses[1]["result"].(map[string]any)["serverInfo"].(map[string]any); info["name"] != "assistant-sessions" {
		t.Errorf("initialize = %v", responses[1])
	}
	tools, _ := responses[2]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 4 {
		t.Errorf("tools/list = %v", tools)
	}
	text := toolText(t, responses[3])
	if !strings.Contains(text, "node-1/-home-ge-chat-chat-1a2b/s-chat") || !strings.Contains(text, "会话=c-1a2b3c4d") {
		t.Errorf("session_search 文本 = %s", text)
	}
	if text := toolText(t, responses[4]); !strings.Contains(text, "来源=chat") || !strings.Contains(text, "标题：chat-1a2b") {
		t.Errorf("session_list 文本 = %s", text)
	}
	if text := toolText(t, responses[5]); !strings.Contains(text, "你好，帮我查队列") {
		t.Errorf("session_read 文本 = %s", text)
	}
	if text := toolText(t, responses[6]); !strings.Contains(text, "c-1a2b3c4d 记录 1 条") {
		t.Errorf("conversation_list 文本 = %s", text)
	}
	if text := toolText(t, responses[7]); !strings.Contains(text, "未知工具") {
		t.Errorf("未知工具应返回错误文本：%s", text)
	}
	if _, ok := responses[8]["error"].(map[string]any); !ok {
		t.Errorf("未知方法应返回 JSON-RPC error：%v", responses[8])
	}
}

// RunMCP 的协议边界：坏 JSON 行被忽略、notification 不回包、未知方法报错、
// tools/call 的参数错误与工具报错都走 isError、EOF 正常收尾。
func TestRunMCPProtocolEdges(t *testing.T) {
	index := seedIndex(t)
	lines := []string{
		`这不是 JSON`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"不是对象"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"session_search","arguments":{"query":""}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"session_search","arguments":"不是对象"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"session_search","arguments":{"query":"没有这句话"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"session_list","arguments":{"limit":"2"}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"conversation_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"no/such/method"}`,
	}
	var out bytes.Buffer
	if err := RunMCP(t.Context(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, MCPOptions{Index: index, Version: "test"}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}

	// 逐行解析回包：坏行与 notification 都没有回包
	decoded := make(map[float64]map[string]any)
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("回包不是 JSON：%s", scanner.Text())
		}
		id, _ := response["id"].(float64)
		decoded[id] = response
	}
	if len(decoded) != 11 {
		t.Errorf("回包数量 = %d，应为 11（坏行与 notification 不回）", len(decoded))
	}
	for _, wantID := range []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		if _, ok := decoded[wantID]; !ok {
			t.Fatalf("缺 id=%v 的回包：%v", wantID, decoded)
		}
	}
	result, _ := decoded[1]["result"].(map[string]any)
	if result == nil {
		t.Fatalf("initialize 回包 = %+v", decoded[1])
	}
	serverInfo, _ := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "assistant-sessions" || serverInfo["version"] != "test" {
		t.Errorf("serverInfo = %+v", serverInfo)
	}
	// params 本身不是对象：协议层 error；参数语义错误（空 query）走工具 error
	if decoded[4]["error"] == nil {
		t.Errorf("params 解析失败应走 error：%+v", decoded[4])
	}
	if inner, _ := decoded[5]["result"].(map[string]any); inner == nil || inner["isError"] != true {
		t.Errorf("空 query 应走工具 error：%+v", decoded[5])
	}
	inner, _ := decoded[6]["result"].(map[string]any)
	if inner == nil || inner["isError"] != true {
		t.Errorf("工具失败应走 isError result：%+v", decoded[6])
	}
	if inner, _ := decoded[10]["result"].(map[string]any); inner == nil || inner["isError"] != true {
		t.Errorf("未知工具应走工具 error：%+v", decoded[10])
	}
	if decoded[11]["error"] == nil {
		t.Errorf("未知方法应走协议 error：%+v", decoded[11])
	}
	// 空结果文案（无命中的检索）
	if text := toolText(t, decoded[7]); !strings.Contains(text, "没有") {
		t.Errorf("id=7 空结果文案 = %q", text)
	}
	// conversation_list 有一条会话实体（limit 字符串也被正确解析成数字）
	if text := toolText(t, decoded[9]); !strings.Contains(text, "c-1a2b3c4d 记录 1 条") {
		t.Errorf("id=9 会话实体文案 = %q", text)
	}
	if text := toolText(t, decoded[8]); !strings.Contains(text, "来源=chat") {
		t.Errorf("id=8 字符串 limit 应生效 = %q", text)
	}
}

// 索引未启用（nil）时：工具返回可读错误，协议层不 panic。
func TestRunMCPWithoutIndex(t *testing.T) {
	var out bytes.Buffer
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_list","arguments":{}}}` + "\n"
	if err := RunMCP(t.Context(), strings.NewReader(request), &out, MCPOptions{}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	if !strings.Contains(out.String(), "isError") || !strings.Contains(out.String(), "索引未启用") {
		t.Errorf("未配置索引时应返回工具错误：%s", out.String())
	}
}

// toolText 取 tools/call 返回的第一段文本。
func toolText(t *testing.T, response map[string]any) string {
	t.Helper()
	result, _ := response["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("响应没有 content：%v", response)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}
