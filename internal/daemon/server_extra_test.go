package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/statestore"
)

// TestServeHealthzAndLimitTruncation 钉住状态 API 的两条此前零覆盖路径：
// /healthz 无鉴权（供探活脚本用，任何 401 都会让容器被误判不健康）与
// /api/v1/results?limit= 的截断（limit 超过实际条数时不报错、limit<=0 返回
// 全量）。这里用直接构造的 Store 快照喂入，不依赖任何外部进程。
func TestServeHealthzAndLimitTruncation(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	store := NewStore("v-srv")
	host, repository := "https://gitea.example.com", "acme/repo"
	for number := int64(1); number <= 3; number++ {
		store.SetQueue(host, repository, time.Now(), []Item{{Kind: "pull", Number: number, Title: "x"}})
		store.Start(host, repository, Item{Kind: "pull", Number: number, Title: "x"}, time.Now())
		store.Finish(host, repository, Item{Kind: "pull", Number: number, Title: "x"}, Result{Subtype: "success"})
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	endpoint, err := Serve(ctx, "127.0.0.1:0", "", store, "v-srv", t.Logf)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// /healthz 不带 Authorization 也必须 200（探活）
	health, err := http.Get("http://" + endpoint.Addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("/healthz 状态 = %d（应无鉴权 200）", health.StatusCode)
	}

	client := NewClient(*endpoint)
	all, err := client.Results(ctx, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("limit=0 应返回全量 3 条：%v %d", err, len(all))
	}
	some, err := client.Results(ctx, 2)
	if err != nil || len(some) != 2 {
		t.Fatalf("limit=2 应截断到 2 条：%v %d", err, len(some))
	}
	// limit 大于总数不报错，原样返回
	over, err := client.Results(ctx, 99)
	if err != nil || len(over) != 3 {
		t.Fatalf("limit 超出总数应原样返回：%v %d", err, len(over))
	}

	// 无鉴权访问受保护端点必须 401
	unauth, err := http.Get("http://" + endpoint.Addr + "/api/v1/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Errorf("无令牌访问应 401，实际 %d", unauth.StatusCode)
	}
	// /api/v1/sessions 与 /api/v1/queue 走各自的处理器
	if _, err := client.Sessions(ctx); err != nil {
		t.Errorf("Sessions: %v", err)
	}
	if _, err := client.Queue(ctx); err != nil {
		t.Errorf("Queue: %v", err)
	}
}

// TestServeStateEndpointRequiresStore 钉住 /api/v1/state 的注册条件：只有传了
// 非 nil 的 statestore.Store 才挂载该路由（没配 state-dir 时访问它应 404 而不
// 是 500）——否则用户会以为状态库坏了。
func TestServeStateEndpointRequiresStore(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	store := NewStore("v-state")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	without, err := Serve(ctx, "127.0.0.1:0", "", store, "v-state", nil)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	response, err := http.Get("http://" + without.Addr + "/api/v1/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("未传 state store 时应 404，实际 %d", response.StatusCode)
	}

	stateStore, err := statestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("statestore.Open: %v", err)
	}
	defer stateStore.Close()

	with, err := Serve(ctx, "127.0.0.1:0", "", store, "v-state", nil, stateStore)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+with.Addr+"/api/v1/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+with.Token)
	stateResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer stateResponse.Body.Close()
	if stateResponse.StatusCode != http.StatusOK {
		t.Errorf("传了 state store 时应 200，实际 %d", stateResponse.StatusCode)
	}
}

// TestServeListenFailure 钉住监听失败路径：地址被占用时 Serve 必须立刻返回
// 错误并带走已分配的 listener（不能泄漏 fd），错误里点名地址。daemon 启动脚本
// 靠这条错误判断「端口被别的实例占了」。
func TestServeListenFailure(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	_, err = Serve(t.Context(), listener.Addr().String(), "", NewStore("v"), "v", nil)
	if err == nil || !strings.Contains(err.Error(), "监听") || !strings.Contains(err.Error(), listener.Addr().String()) {
		t.Errorf("端口占用应报「监听 <addr>」：%v", err)
	}
}

// TestWriteEndpointErrorPropagates 钉住端点文件写入失败的传播：路径上有一层
// 是普通文件时 MkdirAll 失败，Serve 必须报错退出而不是静默继续——端点文件是
// MCP 自举的唯一凭据，写不出去等于 daemon 不可发现。
func TestWriteEndpointErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 在文件名下再建目录 → MkdirAll 必然失败（ENOTDIR）
	err := writeEndpoint(filepath.Join(blocker, "daemon.json"), &Endpoint{Addr: "1.2.3.4:5", Token: "t"})
	if err == nil {
		t.Fatal("路径被文件挡住时 writeEndpoint 应报错")
	}

	// 正常路径：文件权限必须是 0600（含 API 令牌）
	ok := filepath.Join(dir, "ok", "daemon.json")
	if err := writeEndpoint(ok, &Endpoint{Addr: "1.2.3.4:5", Token: "t"}); err != nil {
		t.Fatalf("writeEndpoint: %v", err)
	}
	info, err := os.Stat(ok)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("端点文件权限 = %o（令牌落盘必须 0600）", mode)
	}
}
