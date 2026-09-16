package sessionstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func osMkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }

func fileModTime(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UTC().Format(time.RFC3339Nano)
}

const sampleTranscript = `{"type":"last-prompt","lastPrompt":"你好"}
{"type":"user","message":{"role":"user","content":"你好，帮我查队列"},"timestamp":"2026-09-15T11:31:04Z"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"先看队列"},{"type":"tool_use","name":"mcp__daemon__daemon_status","input":{"a":1}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"没有待办"}]}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"当前没有待评审的 PR"}],"model":"MiniMax-M3"}}
{"type":"system","subtype":"api_error","error":{"message":"HTTP 401 invalid api key"}}`

// 记录原样存储，查询时才抽取可读消息：user/assistant/thinking/tool/tool_result/error。
func TestExtractMessages(t *testing.T) {
	lines := strings.Split(sampleTranscript, "\n")
	var roles []string
	var texts []string
	for index, line := range lines {
		for _, message := range ExtractMessages([]byte(line), index) {
			roles = append(roles, message.Role)
			texts = append(texts, message.Text)
		}
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"你好，帮我查队列", "先看队列", "mcp__daemon__daemon_status", "没有待办", "当前没有待评审的 PR", "HTTP 401 invalid api key"} {
		if !strings.Contains(joined, want) {
			t.Errorf("抽取文本缺少 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "lastPrompt") || strings.Contains(joined, "你好\n") {
		t.Errorf("元数据行不该被当成消息：%s", joined)
	}
	if strings.Join(roles, ",") != "user,thinking,tool,tool_result,assistant,error" {
		t.Errorf("roles = %v", roles)
	}
	first, last := Summarize(lines)
	if first != "你好，帮我查队列" || last != "当前没有待评审的 PR" {
		t.Errorf("Summarize = %q / %q", first, last)
	}
	if DetectSource("assistant-gitea.example.com-acme-video") != "review" ||
		DetectSource("assistant-chat") != "chat" ||
		DetectSource("-home-ge-chat-chat-1a2b") != "chat" ||
		DetectSource("somewhere-else") != "unknown" {
		t.Error("DetectSource 判定不对")
	}
}

// 存/列/读/检：元数据可过滤，检索按子串命中并带上会话键与行号。
func TestStorePutListReadSearch(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := Session{
		Meta: Meta{
			Key:          Key{Host: "node-1", Project: "assistant-gitea.example.com-acme-video", Session: "s-1"},
			Source:       "review",
			UpdatedAt:    "2026-09-15T11:31:04Z",
			Conversation: "", // 评审会话没有会话实体
		},
		Lines: strings.Split(sampleTranscript, "\n"),
	}
	meta, err := store.Put(session)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Lines != len(session.Lines) || meta.Bytes == 0 {
		t.Errorf("meta = %+v", meta)
	}
	if _, err := store.Put(Session{Meta: Meta{Key: Key{Host: "../evil", Project: "p", Session: "s"}}}); err == nil {
		t.Error("非法键应被拒绝")
	}

	chat := Session{
		Meta: Meta{
			Key:          Key{Host: "node-1", Project: "-home-ge-chat-chat-1a2b", Session: "s-2"},
			Source:       "chat",
			Conversation: "c-1a2b3c4d",
			Transport:    "weixin",
			UpdatedAt:    "2026-09-15T12:00:00Z",
		},
		Lines: []string{`{"type":"user","message":{"role":"user","content":"持久记忆怎么做"}}`},
	}
	if _, err := store.Put(chat); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List(Filter{Source: "chat"}, 0)
	if err != nil || len(metas) != 1 || metas[0].Conversation != "c-1a2b3c4d" {
		t.Fatalf("List = %+v err=%v", metas, err)
	}
	if metas, err := store.List(Filter{Conversation: "c-1a2b3c4d"}, 0); err != nil || len(metas) != 1 {
		t.Fatalf("按会话实体过滤失败：%+v err=%v", metas, err)
	}
	messages, err := store.Read(chat.Key, 0, 0)
	if err != nil || len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("Read = %+v err=%v", messages, err)
	}
	if messages, err := store.Read(chat.Key, 1, 0); err != nil || len(messages) != 0 {
		t.Fatalf("offset 应跳过：%+v err=%v", messages, err)
	}
	matches, err := store.Search("待评审", Filter{Source: "review"}, 10)
	if err != nil || len(matches) != 1 || matches[0].Key.Session != "s-1" {
		t.Fatalf("Search = %+v err=%v", matches, err)
	}
	if matches[0].Role != "assistant" || matches[0].Line == 0 {
		t.Errorf("命中信息不全：%+v", matches[0])
	}
	if _, err := store.Search("  ", Filter{}, 5); err == nil {
		t.Error("空检索词应报错")
	}
	counts, err := store.Conversations()
	if err != nil || counts["c-1a2b3c4d"] != 1 {
		t.Fatalf("Conversations = %v err=%v", counts, err)
	}
}

// 客户端与服务端约定：HTTP 往返（这里用最小假服务端，真实 handler 在 cmd 里测）。
func TestClientRoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("authorization") != "Bearer tok" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
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
		case request.URL.Path == "/api/v1/search":
			matches, _ := store.Search(request.URL.Query().Get("q"), Filter{}, 10)
			_ = json.NewEncoder(writer).Encode(map[string]any{"matches": matches})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "tok")
	batch := Batch{Sessions: []Session{{
		Meta:  Meta{Key: Key{Host: "n1", Project: "p1", Session: "s1"}, Source: "chat", Conversation: "c-1"},
		Lines: strings.Split(sampleTranscript, "\n"),
	}}}
	result, err := client.Push(context.Background(), batch)
	if err != nil || result.Stored != 1 {
		t.Fatalf("Push = %+v err=%v", result, err)
	}
	matches, err := client.Search(context.Background(), "队列", Filter{}, 5)
	if err != nil || len(matches) == 0 {
		t.Fatalf("Search = %+v err=%v", matches, err)
	}
	if _, err := NewClient(server.URL, "bad").Search(context.Background(), "x", Filter{}, 5); err == nil {
		t.Error("错误令牌应报错")
	}
	if _, err := NewClient("", "").Push(context.Background(), Batch{}); err == nil {
		t.Error("没有地址应报错")
	}
}

// 采集：按 (host, project, session) 归档，聊天会话带上会话实体与通道；增量跳过未变文件。
func TestCollectAndSessionFor(t *testing.T) {
	configDir := t.TempDir()
	root := filepath.Join(configDir, "claude")
	projectDir := filepath.Join(root, "projects", "-home-ge-chat-chat-1a2b")
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "s-chat.jsonl"), sampleTranscript); err != nil {
		t.Fatal(err)
	}
	chatDir := filepath.Join(configDir, "chat", "chat-1a2b")
	if err := osMkdirAll(chatDir); err != nil {
		t.Fatal(err)
	}
	metadata := `{"conversation_id":"c-1a2b3c4d","session_id":"s-chat","title":"chat-1a2b","model":"MiniMax-M3","transport":"weixin","workdir":"/tmp/chat-1a2b"}`
	if err := writeFile(filepath.Join(chatDir, ChatSessionFile), metadata); err != nil {
		t.Fatal(err)
	}

	options := CollectOptions{Root: root, Host: "node-1", ChatDir: filepath.Join(configDir, "chat"), StatePath: filepath.Join(configDir, "session-push.json")}
	batch, err := Collect(options)
	if err != nil || len(batch.Sessions) != 1 {
		t.Fatalf("Collect = %+v err=%v", batch, err)
	}
	session := batch.Sessions[0]
	if session.Source != "chat" || session.Conversation != "c-1a2b3c4d" || session.Transport != "weixin" || session.Model != "MiniMax-M3" {
		t.Errorf("聊天元数据未带上：%+v", session.Meta)
	}
	if session.FirstUserText == "" || session.LastAssistantText == "" {
		t.Errorf("摘要为空：%+v", session.Meta)
	}

	// 增量：写入状态后第二次采集为空
	state := State{session.Key.String(): {Size: int64(len(sampleTranscript)), ModTime: fileModTime(t, filepath.Join(projectDir, "s-chat.jsonl"))}}
	if err := state.Save(options.StatePath); err != nil {
		t.Fatal(err)
	}
	if batch, err := Collect(options); err != nil || len(batch.Sessions) != 0 {
		t.Fatalf("增量应跳过未变文件：%+v err=%v", batch, err)
	}
	if batch, err := Collect(CollectOptions{Root: root, Host: "node-1", ChatDir: options.ChatDir, All: true}); err != nil || len(batch.Sessions) != 1 {
		t.Fatalf("--all 应重新采集：%+v err=%v", batch, err)
	}

	single, ok, err := SessionFor(options, "s-chat")
	if err != nil || !ok || single.Key.Session != "s-chat" || single.Conversation != "c-1a2b3c4d" {
		t.Fatalf("SessionFor = %+v ok=%v err=%v", single.Meta, ok, err)
	}
	if _, ok, _ := SessionFor(options, "absent"); ok {
		t.Error("不存在的会话应返回 ok=false")
	}
}

// 记录库的身份：目录名不算身份，manifest.json 才算。非空且无 manifest 的目录被拒绝，
// --force 才能接管；格式不符也拒绝（避免把别人的目录当记录库）。
func TestStoreIdentityGuard(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")

	// 空目录/不存在：直接初始化并写 manifest
	store, err := Open(root, OpenOptions{Host: "node-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	manifest := store.Manifest()
	if manifest.Format != Format || manifest.Version != FormatVersion || manifest.Host != "node-1" || manifest.Layout == "" {
		t.Errorf("manifest = %+v", manifest)
	}
	data, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil || !json.Valid(data) {
		t.Fatalf("manifest 未写出：%v", err)
	}

	// 再次打开：读回同一身份
	again, err := Open(root, OpenOptions{})
	if err != nil || again.Manifest().Host != "node-1" {
		t.Fatalf("重新打开失败：%+v err=%v", again, err)
	}

	// 非空、无 manifest：拒绝（这正是 /srv/sessions 被别的程序占用的场景）
	foreign := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "something-else.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(foreign, OpenOptions{}); err == nil || !strings.Contains(err.Error(), "非空") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("非空无 manifest 的目录应被拒绝：%v", err)
	}
	if _, err := Open(foreign, OpenOptions{Host: "node-2", Force: true}); err != nil {
		t.Fatalf("--force 应能接管：%v", err)
	}

	// 格式不符：即使有 manifest 也拒绝
	mismatched := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(mismatched, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mismatched, ManifestFile), []byte(`{"format":"someone.else","version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(mismatched, OpenOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "someone.else") {
		t.Fatalf("格式不符应被拒绝（--force 也不行）：%v", err)
	}
}
