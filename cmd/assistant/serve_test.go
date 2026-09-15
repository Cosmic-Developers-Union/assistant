package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/sessionstore"
)

// 服务端：令牌鉴权、push/list/read/search/conversations 全链路（借 sessionstore 客户端）。
func TestServeHandlerRoundTrip(t *testing.T) {
	store, err := sessionstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(serveHandler(store, "tok", func(string, ...any) {}))
	defer server.Close()

	if response, err := server.Client().Get(server.URL + "/healthz"); err != nil || response.StatusCode != 200 {
		t.Fatalf("healthz 应免鉴权：%v %v", response, err)
	}
	unauthorized := sessionstore.NewClient(server.URL, "bad")
	if _, err := unauthorized.List(context.Background(), sessionstore.Filter{}, 0); err == nil {
		t.Error("错误令牌应被拒")
	}

	client := sessionstore.NewClient(server.URL, "tok")
	key := sessionstore.Key{Host: "node-1", Project: "assistant-gitea.example.com-acme-video", Session: "s-1"}
	if result, err := client.Push(context.Background(), sessionstore.Batch{Sessions: []sessionstore.Session{{
		Meta:  sessionstore.Meta{Key: key, Source: "review"},
		Lines: []string{`{"type":"user","message":{"role":"user","content":"帮我评审 #12"}}`},
	}}}); err != nil || result.Stored != 1 {
		t.Fatalf("Push = %+v err=%v", result, err)
	}
	metas, err := client.List(context.Background(), sessionstore.Filter{Host: "node-1"}, 0)
	if err != nil || len(metas) != 1 || metas[0].Lines != 1 {
		t.Fatalf("List = %+v err=%v", metas, err)
	}
	messages, err := client.Read(context.Background(), key, 0, 0)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Text, "#12") {
		t.Fatalf("Read = %+v err=%v", messages, err)
	}
	matches, err := client.Search(context.Background(), "评审", sessionstore.Filter{}, 5)
	if err != nil || len(matches) != 1 {
		t.Fatalf("Search = %+v err=%v", matches, err)
	}
	counts, err := client.Conversations(context.Background())
	if err != nil || len(counts) != 0 {
		t.Fatalf("Conversations = %v err=%v", counts, err)
	}
	// 非法键（带路径分隔符）被拒绝
	if _, err := client.Push(context.Background(), sessionstore.Batch{Sessions: []sessionstore.Session{{
		Meta:  sessionstore.Meta{Key: sessionstore.Key{Host: "../evil", Project: "p", Session: "s"}},
		Lines: []string{"{}"},
	}}}); err == nil {
		t.Error("非法键应被拒")
	}
}

// session push：扫描本地记录 → 推送；增量第二次为空；--dry-run 不发请求。
func TestSessionPushCommand(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	claudeRoot := filepath.Join(configDir, "claude")
	projectDir := filepath.Join(claudeRoot, "projects", "-home-ge-chat-chat-1a2b")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","message":{"role":"user","content":"持久化怎么做"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"存到记录库"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "s-chat.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	chatDir := filepath.Join(configDir, "chat", "chat-1a2b")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"conversation_id":"c-1a2b3c4d","session_id":"s-chat","title":"chat-1a2b","transport":"weixin"}`
	if err := os.WriteFile(filepath.Join(chatDir, "session.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := sessionstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(serveHandler(store, "tok", func(string, ...any) {}))
	defer server.Close()

	run := func(arguments ...string) (string, error) {
		command := newSessionCommand(&configPath)
		command.SetArgs(append([]string{"push", "--root", claudeRoot, "--host", "node-1", "--url", server.URL, "--token", "tok"}, arguments...))
		output := &bytes.Buffer{}
		command.SetOut(output)
		command.SetErr(output)
		err := command.Execute()
		return output.String(), err
	}

	output, err := run()
	if err != nil {
		t.Fatalf("push: %v\n%s", err, output)
	}
	if !strings.Contains(output, "已推送 1 条会话记录") || !strings.Contains(output, "来源 chat") {
		t.Errorf("输出 = %s", output)
	}
	client := sessionstore.NewClient(server.URL, "tok")
	matches, err := client.Search(context.Background(), "记录库", sessionstore.Filter{}, 5)
	if err != nil || len(matches) != 1 || matches[0].Conversation != "c-1a2b3c4d" || matches[0].Transport != "weixin" {
		t.Fatalf("推送后的记录应带会话实体/通道：%+v err=%v", matches, err)
	}

	if output, err := run(); err != nil || !strings.Contains(output, "没有新记录需要推送") {
		t.Fatalf("第二次应为增量空转：err=%v\n%s", err, output)
	}
	if output, err := run("--all"); err != nil || !strings.Contains(output, "已推送 1 条") {
		t.Fatalf("--all 应重推：err=%v\n%s", err, output)
	}
	if output, err := run("--all", "--dry-run", "--url", "http://127.0.0.1:1"); err != nil || !strings.Contains(output, "dry-run：1 条待推送") {
		t.Fatalf("dry-run 不该发请求：err=%v\n%s", err, output)
	}
}

// sessions MCP 也能从 serve.json 自举（同机部署的关键路径）。
func TestResolveRemoteFromServeFile(t *testing.T) {
	configDir := t.TempDir()
	endpoint := serveEndpoint{URL: "http://127.0.0.1:8780", Token: "tok", Root: "/tmp/sessions"}
	path, err := writeServeEndpoint(configDir, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != sessionstore.ServeFile {
		t.Errorf("端点文件名 = %s", path)
	}
	remote := sessionstore.ResolveRemote(configDir)
	if remote.URL != endpoint.URL || remote.Token != "tok" {
		t.Errorf("ResolveRemote = %+v", remote)
	}
	// 远端配置优先于本机 serve.json
	remoteFile := `{"url":"https://sessions.example.com","token":"remote"}`
	if err := os.WriteFile(filepath.Join(configDir, sessionstore.RemoteFile), []byte(remoteFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if resolved := sessionstore.ResolveRemote(configDir); resolved.URL != "https://sessions.example.com" {
		t.Errorf("sessions-remote.json 应优先：%+v", resolved)
	}
	if data, err := os.ReadFile(filepath.Join(configDir, sessionstore.ServeFile)); err != nil || !json.Valid(data) {
		t.Errorf("serve.json 应是合法 JSON：%v", err)
	}
}
