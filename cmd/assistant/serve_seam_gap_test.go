package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"
	"github.com/spf13/cobra"
)

// serveGapOptions 造一份「一切落点都在临时目录里」的 serveOptions：
// 记录库、配置目录、令牌都显式给全，只有被测的那一项留空由用例自己设。
//
// 这样每条用例只表达「哪一处失败」，而不会被缺省落点（真实用户目录）干扰。
func serveGapOptions(t *testing.T) (*serveOptions, string) {
	t.Helper()
	configDir := t.TempDir()
	return &serveOptions{
		Listen: "127.0.0.1:0",
		Root:   filepath.Join(t.TempDir(), "sessions"),
		Host:   "gap-host",
		Token:  "gap-token",
		Quiet:  true,
	}, filepath.Join(configDir, "config.json")
}

// serveGapCommand 造一个只接输出缓冲的命令：runServe 的输出必须能被断言，
// 否则失败路径报什么、引导写没有都无从检查。
func serveGapCommand(t *testing.T, out *bytes.Buffer) *cobra.Command {
	t.Helper()
	command := &cobra.Command{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetContext(t.Context())
	return command
}

// TestRunServeReportsUnresolvableWorkingDirectory 断言缺省记录库目录取不出来时
// serve 立刻失败，绝不先去监听端口。
//
// 缺省根目录跟随当前目录（data/sessions）。工作目录被删掉时继续跑会把记录库落到
// 进程某个谁也说不清的位置：客户端照样能连上、照样能推记录，而运维重启后到处都
// 找不到那批数据。必须在监听之前就失败。
func TestRunServeReportsUnresolvableWorkingDirectory(t *testing.T) {
	isolateCredentials(t)
	out := &bytes.Buffer{}
	options, configPath := serveGapOptions(t)
	// Root 留空 → 必须靠 Getwd 定位；Getwd 失败时这条路径就是唯一出路
	options.Root = ""
	options.Getwd = func() (string, error) { return "", os.ErrPermission }

	err := runServe(serveGapCommand(t, out), configPath, options)
	if err == nil {
		t.Fatalf("取不出工作目录时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "permission") {
		t.Errorf("错误应透出底层原因：%v", err)
	}
}

// TestRunServeReportsTokenGenerationFailure 断言访问令牌生成失败时 serve 立刻
// 失败，既不监听也不留下端点文件。
//
// 端点文件（serve.json）是客户端与 agent MCP 自举的唯一凭据来源。若在令牌取不到
// 的情况下仍然监听并落盘，客户端会读到一份没有令牌（或带空令牌）的端点，之后每条
// 请求都得到 401——而真正的故障（随机源坏了）被掩盖成一个「令牌不对」的谜题。
func TestRunServeReportsTokenGenerationFailure(t *testing.T) {
	isolateCredentials(t)
	configDir := t.TempDir()
	out := &bytes.Buffer{}
	options, _ := serveGapOptions(t)
	options.Token = ""
	options.Rand = func([]byte) (int, error) { return 0, os.ErrClosed }

	configPath := filepath.Join(configDir, "config.json")
	err := runServe(serveGapCommand(t, out), configPath, options)
	if err == nil {
		t.Fatalf("令牌生成失败时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "file already closed") {
		t.Errorf("错误应透出随机源的失败：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(configDir, sessionstore.ServeFile)); !os.IsNotExist(statErr) {
		t.Errorf("失败时不该留下端点文件：%v", statErr)
	}
}

// TestRunServeReportsUnwritableEndpointDir 断言端点目录建不出来（被普通文件占住）
// 时 serve 立刻失败，不会照常开始监听。
//
// 这是最隐蔽的一种启动成功：监听端口没问题、记录库也没问题，只有端点文件写不下去。
// 于是服务看上去运行正常，而所有同机客户端和 agent MCP 都永远发现不了它——用户看到
// 的是「MCP 工具一直说未配置」，排查方向完全错位。
func TestRunServeReportsUnwritableEndpointDir(t *testing.T) {
	isolateCredentials(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("占位文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	options, _ := serveGapOptions(t)

	// configPath 落在被文件占住的路径下 → resolveConfigDir 得到 blocked，MkdirAll 必失败
	configPath := filepath.Join(blocked, "config.json")
	err := runServe(serveGapCommand(t, out), configPath, options)
	if err == nil {
		t.Fatalf("端点目录建不出来时应报错：\n%s", out.String())
	}
	if strings.Contains(out.String(), "监听：") {
		t.Errorf("不该已经开始监听：\n%s", out.String())
	}
}

// TestRunServeCleansEndpointWhenCancelledBeforeServing 断言「端点文件刚写完就被取消」
// 这条时序下 serve 仍然把端点文件清掉，不给客户端留下一份指向死服务的配置。
//
// 端点文件一旦落盘，客户端就会照它去连。若 serve 因为取消而提前退出却把文件留着，
// 之后每个同机客户端都会连到一个不存在的端口——报出来的是「连接被拒绝」，而真正的
// 原因是那次启动根本没跑起来。清理由 defer 保证，这条钉住的就是它确实生效。
func TestRunServeCleansEndpointWhenCancelledBeforeServing(t *testing.T) {
	isolateCredentials(t)
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	out := &bytes.Buffer{}
	options, _ := serveGapOptions(t)

	ctx, cancel := context.WithCancel(t.Context())
	command := serveGapCommand(t, out)
	command.SetContext(ctx)
	// 文件写完的确定时点：此刻取消，runServe 会在进入 select 前就看到 ctx 结束
	previous := serveEndpointWritten
	serveEndpointWritten = func() {
		serveEndpointWritten = previous
		cancel()
	}
	t.Cleanup(func() { serveEndpointWritten = previous })

	err := runServe(command, configPath, options)
	if err != nil {
		t.Fatalf("取消不该报错：%v\n%s", err, out.String())
	}
	endpointPath := filepath.Join(configDir, sessionstore.ServeFile)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, statErr := os.Stat(endpointPath)
		if os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("端点文件 %s 退出后仍存在（没被清理）", endpointPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// brokenStore 造一个「打开了、但写不进去」的记录库：库根在打开后被普通文件顶替，
// 于是 Put 落盘时必然失败。
//
// 覆盖 serve 的写入失败分支需要它——列表/读取路径不经过 Put，只有真的写一次才会
// 走到那条错误处理。
func brokenStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := sessionstore.Open(root, sessionstore.OpenOptions{Host: "gap-host"})
	if err != nil {
		t.Fatal(err)
	}
	// 用文件顶替库根：Put 需要在其下建目录/写文件，必然失败
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("占位"), 0o600); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestHandleSessionPushReportsPartialStore 断言入库中途失败时返回 400 并且
// 明确带上「已经存下几条」。
//
// 客户端据此决定要不要重推整批：退回一个没有 stored 的响应，客户端无法区分
// 「一条都没存」和「存了一半」，只能整批重推，于是已成功的记录被写第二遍。
func TestHandleSessionPushReportsPartialStore(t *testing.T) {
	store := brokenStore(t)
	body, err := json.Marshal(sessionstore.Batch{Sessions: []sessionstore.Session{{}}})
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader(body))
	handleSessionPush(recorder, request, store, func(string, ...any) {})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("写入失败应回 400，got %d：%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["error"]; !ok {
		t.Errorf("响应应带 error：%s", recorder.Body.String())
	}
	if stored, ok := payload["stored"].(float64); !ok || stored != 0 {
		t.Errorf("响应应带已存条数 stored=0：%s", recorder.Body.String())
	}
}

// TestHandleSessionPushAcceptsEmptyBatch 断言空批次返回 200 且 stored=0。
//
// 拉取端在「没有新记录」时会照常发起一次推送，空批是正常请求而不是客户端错误。
// 若这里回 4xx，客户端会把每次空闲轮询都记成失败并开始重试，日志被噪音淹没。
func TestHandleSessionPushAcceptsEmptyBatch(t *testing.T) {
	store := brokenStore(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"sessions":[]}`))
	handleSessionPush(recorder, request, store, func(string, ...any) {})

	if recorder.Code != http.StatusOK {
		t.Fatalf("空批次应回 200，got %d：%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if stored, ok := payload["stored"].(float64); !ok || stored != 0 {
		t.Errorf("空批次 stored 应为 0：%s", recorder.Body.String())
	}
}

// TestServeHandlerReportsStoreFailures 断言库根读不了时，只读接口以明确的错误
// 状态回应，而不是回 200 加一个空结果。检索另有一条「检索词为空」的请求错误。
//
// 空结果与「真的没有记录」在客户端看来一模一样：agent 会得出「这台机器上没跑过
// 任何会话」的结论，而真正的原因是记录库读不了。错误必须冒到状态码上。
func TestServeHandlerReportsStoreFailures(t *testing.T) {
	isolateCredentials(t)

	// 库根整个消失：WalkDir 在根上就 lstat 失败，列表/会话归类必然出错。
	// 必须先让 Open 正常开起来，再删掉它——Open 自己会读 manifest，根不在就开不了。
	blockedRoot := filepath.Join(t.TempDir(), "blocked")
	blockedStore, err := sessionstore.Open(blockedRoot, sessionstore.OpenOptions{Host: "gap-host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(blockedRoot); err != nil {
		t.Fatal(err)
	}
	blockedAddress := newLocalServer(t, serveHandler(blockedStore, "gap-token", func(string, ...any) {}))

	for _, testCase := range []struct {
		name string
		url  string
		want int
	}{
		// 库读不了时列表必须报 500：不能回一份空列表冒充「没有记录」
		{"列表读不了库", blockedAddress + "/api/v1/sessions", http.StatusInternalServerError},
		// 会话归类走同一条列表路径，同样是服务端错误
		{"会话归类读不了库", blockedAddress + "/api/v1/conversations", http.StatusInternalServerError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body, status := serveRequest(t, http.MethodGet, testCase.url, "gap-token", "")
			if status != testCase.want {
				t.Errorf("%s 状态码 = %d，want %d：%s", testCase.name, status, testCase.want, body)
			}
			if !strings.Contains(body, "error") {
				t.Errorf("%s 响应应带 error：%s", testCase.name, body)
			}
		})
	}

	// 检索的空词是请求侧错误：客户端得知道是查询写错了，而不是记录库没内容
	healthyRoot := filepath.Join(t.TempDir(), "sessions")
	healthyStore, err := sessionstore.Open(healthyRoot, sessionstore.OpenOptions{Host: "gap-host"})
	if err != nil {
		t.Fatal(err)
	}
	healthyAddress := newLocalServer(t, serveHandler(healthyStore, "gap-token", func(string, ...any) {}))
	body, status := serveRequest(t, http.MethodGet, healthyAddress+"/api/v1/search?q=", "gap-token", "")
	if status != http.StatusBadRequest {
		t.Errorf("空检索词状态码 = %d，want 400：%s", status, body)
	}
	if !strings.Contains(body, "error") {
		t.Errorf("空检索词响应应带 error：%s", body)
	}
}

// TestServeHandlerReportsUnreadableSession 断言读取一条「meta 在、原样记录不在」
// 的会话时回 404，而不是 200 加空消息列表。
//
// 客户端拿到空列表会认为该会话确实没有内容；而事实是记录文件丢了——这正是备份/
// 迁移后最需要被发现的形态，必须让读取方看见 404。
func TestServeHandlerReportsUnreadableSession(t *testing.T) {
	isolateCredentials(t)
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := sessionstore.Open(root, sessionstore.OpenOptions{Host: "gap-host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(sessionstore.Session{Host: "gap-host", Project: "proj", Session: "sess"}); err != nil {
		t.Fatal(err)
	}
	handler := serveHandler(store, "gap-token", func(string, ...any) {})
	address := newLocalServer(t, handler)

	// 删掉原样记录文件，只留下 meta（列表仍能列出它，读取则必须失败）
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			return os.Remove(path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	body, status := serveRequest(t, http.MethodGet, address+"/api/v1/sessions/gap-host/proj/sess", "gap-token", "")
	if status != http.StatusNotFound {
		t.Errorf("记录文件缺失应回 404，got %d：%s", status, body)
	}
}
