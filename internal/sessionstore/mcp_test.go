package sessionstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer 是最小记录库服务端（真实 handler 在 cmd/assistant 里另测）。
func fakeServer(t *testing.T, store *Store) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/sessions":
			var batch Batch
			_ = json.NewDecoder(request.Body).Decode(&batch)
			for _, session := range batch.Sessions {
				if _, err := store.Put(session); err != nil {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"stored": len(batch.Sessions)})
		case request.URL.Path == "/api/v1/sessions":
			metas, _ := store.List(Filter{}, 0)
			_ = json.NewEncoder(writer).Encode(map[string]any{"sessions": metas})
		case request.URL.Path == "/api/v1/search":
			matches, err := store.Search(request.URL.Query().Get("q"), Filter{}, 10)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"matches": matches})
		case request.URL.Path == "/api/v1/conversations":
			counts, _ := store.Conversations()
			_ = json.NewEncoder(writer).Encode(map[string]any{"conversations": counts})
		case strings.HasPrefix(request.URL.Path, "/api/v1/sessions/"):
			parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/sessions/"), "/")
			messages, err := store.Read(Key{Host: parts[0], Project: parts[1], Session: parts[2]}, 0, 0)
			if err != nil {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"messages": messages})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// MCP 工具：session_search / session_list / session_read / conversation_list 四个工具
// 走真实 HTTP 客户端，返回可直接给 agent 读的文本。
func TestRunMCPTools(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(Session{
		Meta: Meta{
			Key:          Key{Host: "node-1", Project: "-home-ge-chat-chat-1a2b", Session: "s-chat"},
			Source:       "chat",
			Conversation: "c-1a2b3c4d",
			Transport:    "weixin",
			Title:        "chat-1a2b",
		},
		Lines: strings.Split(sampleTranscript, "\n"),
	}); err != nil {
		t.Fatal(err)
	}
	server := fakeServer(t, store)

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
	if err := RunMCP(context.Background(), &input, &output, MCPOptions{Client: NewClient(server.URL, "")}); err != nil {
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
