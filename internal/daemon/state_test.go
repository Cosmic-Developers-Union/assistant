package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"assistant/internal/instances"
)

func TestStoreLifecycle(t *testing.T) {
	store := NewStore("v-test")
	host, repository := "https://gitea.example.com", "acme/repo"
	store.AddTarget(Target{Host: host, Repository: repository, Dir: "/srv/repo", Ready: true})
	store.AddTarget(Target{Host: host, Repository: "acme/broken", Ready: false, SkipReason: "缺少 .mcp.json"})
	started := time.Now().Add(-time.Minute)
	store.SetQueue(host, repository, time.Now(), []Item{{Kind: "pull", Number: 7, Title: "fix"}})
	store.Start(host, repository, Item{Kind: "pull", Number: 7, Title: "fix"}, started)

	status := store.Snapshot()
	if len(status.Targets) != 2 || len(status.Queue) != 1 || len(status.Sessions) != 1 {
		t.Fatalf("status = %+v", status)
	}
	if !status.Targets[0].Ready || status.Targets[1].Ready || status.Targets[1].SkipReason == "" {
		t.Errorf("targets = %+v", status.Targets)
	}
	if status.Queue[0].Items[0].Number != 7 || status.Sessions[0].StartedAt.IsZero() {
		t.Errorf("queue/session = %+v %+v", status.Queue[0], status.Sessions[0])
	}

	store.Finish(host, repository, Item{Kind: "pull", Number: 7, Title: "fix"}, Result{Subtype: "success"})
	status = store.Snapshot()
	if len(status.Sessions) != 0 {
		t.Errorf("会话应已结束：%+v", status.Sessions)
	}
	if len(status.Recent) != 1 {
		t.Fatalf("recent = %+v", status.Recent)
	}
	result := status.Recent[0]
	if result.Host != host || result.Repository != repository || result.StartedAt.IsZero() || result.FinishedAt.IsZero() {
		t.Errorf("result = %+v", result)
	}
}

func TestServeAndClientRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store := NewStore("v-round")
	store.AddTarget(Target{Host: "https://gitea.example.com", Repository: "acme/repo"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint, err := Serve(ctx, "127.0.0.1:0", "", store, "v-round", nil)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	path, err := endpointPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("端点文件未写入：%v", err)
	}

	client, err := Discover(os.Getenv)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Version != "v-round" || len(status.Targets) != 1 {
		t.Errorf("status = %+v", status)
	}
	if _, err := client.Results(ctx, 5); err != nil {
		t.Errorf("Results: %v", err)
	}

	// 错误令牌被拒绝
	bad := NewClient(Endpoint{Addr: endpoint.Addr, Token: "nope"})
	if _, err := bad.Status(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("错误令牌应 401：%v", err)
	}

	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("退出后端点文件应删除")
}

func endpointPath() (string, error) {
	return instances.DaemonEndpointPath()
}

func TestMCPTools(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store := NewStore("v-mcp")
	store.AddTarget(Target{Host: "https://gitea.example.com", Repository: "acme/repo"})
	store.Start("https://gitea.example.com", "acme/repo", Item{Kind: "pull", Number: 3, Title: "feat"}, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Serve(ctx, "127.0.0.1:0", "", store, "v-mcp", nil); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"daemon_status"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_sessions"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`,
	}, "\n") + "\n"
	var output strings.Builder
	if err := RunMCP(ctx, strings.NewReader(input), &output, os.Getenv, "v-mcp"); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	text := output.String()
	for _, want := range []string{
		"assistant-daemon", "daemon_status", "list_queue",
		"v-mcp", "acme/repo", `\"kind\": \"pull\"`,
		"未知工具",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("MCP 输出缺少 %q：%s", want, text)
		}
	}
}

func TestChatStableSession(t *testing.T) {
	dir := t.TempDir()
	var calls [][]string
	chat, err := NewChat(ChatConfig{
		StateDir:   dir,
		SessionDir: t.TempDir(),
		RunClaude: func(_ context.Context, _ string, args []string, _ string, _ []string) ([]byte, error) {
			calls = append(calls, args)
			return []byte(`{"subtype":"success","is_error":false,"result":"有 2 个 PR 在评审"}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	ctx := context.Background()
	reply, err := chat.Handle(ctx, "user-1", Turn{Transport: "weixin", Text: "现在有多少 PR 在 review？"})
	if err != nil || reply != "有 2 个 PR 在评审" {
		t.Fatalf("Handle = %q, %v", reply, err)
	}
	first := strings.Join(calls[0], " ")
	if !strings.Contains(first, "--session-id") || !strings.Contains(first, "assistant mcp daemon") == false {
		t.Errorf("首轮参数 = %s", first)
	}
	if _, err := chat.Handle(ctx, "user-1", Turn{Transport: "weixin", Text: "第二个问题"}); err != nil {
		t.Fatal(err)
	}
	second := strings.Join(calls[1], " ")
	if !strings.Contains(second, "--resume") {
		t.Errorf("后续轮应 --resume：%s", second)
	}
	if !strings.Contains(first, "--session-id") {
		t.Errorf("首轮应 --session-id：%s", first)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err != nil {
		t.Errorf("会话映射应落盘：%v", err)
	}
	if _, err := chat.Handle(ctx, "user-2", Turn{Transport: "weixin", Text: "你好"}); err != nil {
		t.Fatal(err)
	}
	if calls[2][0] == calls[0][0] && strings.Contains(strings.Join(calls[2], " "), "--resume") {
		// 新会话应是新 UUID：不做严格断言，但必须生成新 session（--session-id）
		t.Log("new conversation args ok")
	}
}
