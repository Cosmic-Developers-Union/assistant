package runtime

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/integration"
)

func platformServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("只读运行写入平台: %s %s", r.Method, r.URL)
			http.Error(w, "write", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			fmt.Fprint(w, `{"version":"1.27.0"}`)
		case "/api/v1/user":
			if r.Header.Get("Authorization") != "token test-token" {
				http.Error(w, "denied", 401)
				return
			}
			fmt.Fprint(w, `{"login":"ai"}`)
		case "/api/v1/user/repos":
			fmt.Fprint(w, `[{"name":"repo","owner":{"login":"acme"}}]`)
		case "/api/v1/repos/issues/search", "/api/v1/repos/acme/repo/pulls", "/api/v1/repos/acme/repo/issues":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("未预期的请求: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
func TestStartDryRunAndCancellationAreIndependentOfCredentials(t *testing.T) {
	server := platformServer(t)
	t.Setenv("ASSISTANT_CREDENTIALS", "/missing/forbidden/credentials.json")
	t.Setenv("TOKEN", "test-token")
	cfg, err := Decode([]byte(strings.ReplaceAll(validConfig, "https://gitea.example", server.URL)), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runtime.Root = filepath.Join(t.TempDir(), "not-created")
	var output bytes.Buffer
	log := func(format string, args ...any) { fmt.Fprintf(&output, format, args...) }
	if err := Start(t.Context(), cfg, true, true, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Runtime.Root); !os.IsNotExist(err) {
		t.Fatal("演练创建了运行根")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	cfg.Runtime.APIListen = "127.0.0.1:0"
	cfg.Runtime.Interval = time.Millisecond
	if err := Start(ctx, cfg, false, true, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Runtime.Root, "api.json")); !os.IsNotExist(err) {
		t.Fatal("退出后状态端点残留")
	}
	bot := cfg.Bots["review"]
	bot.With.Identity = "wrong"
	cfg.Bots["review"] = bot
	if err := Start(t.Context(), cfg, true, false, log); err == nil {
		t.Fatal("身份不匹配被接受")
	}
	cfg.Bots = nil
	if err := Start(t.Context(), cfg, true, false, nil); err == nil {
		t.Fatal("无 bot 启动")
	}
}
func TestChatAssemblyAndReset(t *testing.T) {
	for _, platform := range []string{"qq", "weixin", "telegram"} {
		adapter, err := chatAdapter("named", Connect{Type: platform}, nil)
		if err != nil {
			t.Fatal(err)
		}
		wrapped := &namedChat{ChatIntegration: adapter, name: "named"}
		if wrapped.Name() != "named" {
			t.Fatal(wrapped.Name())
		}
	}
	if _, err := chatAdapter("x", Connect{Type: "unknown"}, nil); err == nil {
		t.Fatal("未知适配器被接受")
	}
	root := t.TempDir()
	var ids []string
	var active, maxActive atomic.Int32
	engine := &Engine{Workers: map[string]Worker{"chat": {Workspace: DirectoryWorkspace{Root: filepath.Join(root, "chat", "chat")}, Session: LocalSession{Root: filepath.Join(root, "sessions")}, Runner: runnerFunc(func(ctx context.Context, spec AgentSpec, ev Event, dir, id string) (Outcome, error) {
		count := active.Add(1)
		if count > maxActive.Load() {
			maxActive.Store(count)
		}
		time.Sleep(time.Millisecond)
		ids = append(ids, id)
		active.Add(-1)
		return Outcome{Result: ev.Text}, nil
	})}}}
	bridge := &chatBridge{engine: engine, bot: "chat", root: root}
	id, _ := bridge.ConversationFor("tg", "user")
	first, err := bridge.Handle(t.Context(), id, integration.Turn{Text: "hello"})
	if err != nil || first != "hello" {
		t.Fatal(first, err)
	}
	if _, err := bridge.Handle(t.Context(), id, integration.Turn{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if ids[0] != ids[1] {
		t.Fatal("普通消息未续接")
	}
	work, _ := bridge.WorkspaceDir(id)
	_ = os.WriteFile(filepath.Join(work, "keep"), []byte("file"), 0o600)
	if err := bridge.Reset(id); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Handle(t.Context(), id, integration.Turn{Text: "new"}); err != nil {
		t.Fatal(err)
	}
	if ids[2] == ids[0] {
		t.Fatal("重置未新建记忆")
	}
	if _, err := os.Stat(filepath.Join(work, "keep")); err != nil {
		t.Fatal("重置删除了用户文件")
	}
	// 普通消息对同一用户串行。
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { _, _ = bridge.Handle(t.Context(), id, integration.Turn{Text: "parallel"}); done <- struct{}{} }()
	}
	<-done
	<-done
	if maxActive.Load() != 1 {
		t.Fatal("同用户消息并发执行")
	}
	cfg, err := Decode([]byte("connects:\n  tg: {type: telegram, token: '{{TOKEN}}'}\nbots:\n  chat: {kind: chat, use: {telegram: tg}, workspace: {type: directory}}\n"), lookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runtime.Root = filepath.Join(t.TempDir(), "untouched")
	if err := Start(t.Context(), cfg, true, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Runtime.Root); !os.IsNotExist(err) {
		t.Fatal("演练启动消息通道")
	}
}

func TestStartCancelledChatAndStorageErrors(t *testing.T) {
	for _, platform := range []string{"telegram", "qq", "weixin"} {
		fields := "token: '{{TOKEN}}'"
		if platform == "qq" {
			fields = "app-id: '1', app-secret: '{{TOKEN}}'"
		}
		cfg, err := Decode([]byte("connects:\n  chat: {type: "+platform+", "+fields+"}\nbots:\n  chat: {kind: chat, use: {"+platform+": chat}, workspace: {type: directory}}\n"), lookup)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Runtime.Root = t.TempDir()
		cfg.Runtime.Debug = true
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_ = Start(ctx, cfg, false, true, func(string, ...any) {})
		cfg.Session = SessionConfig{Store: "s3", Endpoint: "https://minio.example", Bucket: "sessions", AccessKey: "access", SecretKey: "secret"}
		if err := Start(ctx, cfg, true, false, nil); err != nil {
			t.Fatal(err)
		}
		cfg.Session.Endpoint = "https://invalid/path"
		if err := Start(t.Context(), cfg, true, false, nil); err == nil {
			t.Fatal("错误 S3 配置被接受")
		}
		cfg.Session = SessionConfig{Store: "local"}
		cfg.Runtime.APIListen = "invalid-address"
		if err := Start(t.Context(), cfg, false, false, nil); err == nil {
			t.Fatal("无效 API 地址被吞掉")
		}
	}
}

func TestRunRootIsSingleProcessOwned(t *testing.T) {
	root := t.TempDir()
	release, err := acquireRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireRoot(root); err == nil {
		t.Fatal("同一运行根被两个进程占用")
	}
	release()
	again, err := acquireRoot(root)
	if err != nil {
		t.Fatal("退出后锁未释放", err)
	}
	again()
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if _, err := acquireRoot(filepath.Join(blocker, "data")); err == nil {
		t.Fatal("无效运行根被接受")
	}
	root = t.TempDir()
	_ = os.Mkdir(filepath.Join(root, "run.lock"), 0o700)
	if _, err := acquireRoot(root); err == nil {
		t.Fatal("无法打开锁的错误被吞掉")
	}
}
