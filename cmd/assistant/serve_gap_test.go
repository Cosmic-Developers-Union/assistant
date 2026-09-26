package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
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

// TestRunServeServesUntilContextCancelled 端到端钉住 runServe 的生命周期契约：
// 监听起来之后必须真的对外服务（healthz 免鉴权、其余接口要 Bearer 令牌），
// 且收到取消信号后走优雅停机而不是把进程卡住——这是 daemon 与 agent 能长期
// 挂着它的前提。
//
// 缺了这条断言（runServe 此前 0% 覆盖）就没有任何测试证明「serve 真的能起来」：
// 端口解析、端点文件落点、令牌鉴权、停机分支任何一处坏掉，都只会在生产上暴露。
func TestRunServeServesUntilContextCancelled(t *testing.T) {
	isolateCredentials(t)
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sessions")

	command := &cobra.Command{}
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	// runServe 阻塞在 select 上，用可取消的上下文把停机分支变成可控的。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command.SetContext(ctx)

	options := &serveOptions{
		Listen: "127.0.0.1:0",
		Root:   root,
		Host:   "node-test",
		Token:  "fixed-token",
	}
	done := make(chan error, 1)
	go func() { done <- runServe(command, configPath, options) }()

	// 端点文件是 runServe 对外发布的唯一凭据来源（同机客户端靠它自举），
	// 同时它出现即代表监听已经就绪。
	endpointPath := filepath.Join(configDir, sessionstore.ServeFile)
	endpoint := waitForServeEndpoint(t, endpointPath)
	if endpoint.Token != "fixed-token" {
		t.Errorf("端点令牌 = %q, want fixed-token", endpoint.Token)
	}
	if endpoint.Root != root {
		t.Errorf("端点记录库 = %q, want %q", endpoint.Root, root)
	}
	if endpoint.PID != os.Getpid() {
		t.Errorf("端点 PID = %d, want 本进程 %d", endpoint.PID, os.Getpid())
	}
	if endpoint.StartedAt == "" {
		t.Error("端点文件应记录启动时间（供人工分辨是不是当前这个实例）")
	}
	if !strings.HasPrefix(endpoint.URL, "http://127.0.0.1:") {
		t.Errorf("端点 URL = %q, want http://127.0.0.1:<port>", endpoint.URL)
	}
	// 监听地址写成 :0 时端点 URL 只做展示用的重写（127.0.0.1+原串），不会回填真实
	// 端口，所以断言要走 listener 的真实地址而非端点文件里的 URL。
	base := "http://" + takeServeListenAddress(t)

	if body, status := serveRequest(t, http.MethodGet, base+"/healthz", "", ""); status != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("healthz = %d %s，应免鉴权且返回 ok", status, body)
	}
	if _, status := serveRequest(t, http.MethodGet, base+"/api/v1/sessions", "", ""); status != http.StatusUnauthorized {
		t.Errorf("无令牌 GET /api/v1/sessions = %d, want 401", status)
	}
	// 带令牌推一条记录：证明监听出来的服务端接的是同一份记录库。
	push := `{"sessions":[{"host":"node-test","project":"p","session":"s","source":"review","pushed_at":"2026-09-26T10:00:00Z","lines":["{}"]}]}`
	if body, status := serveRequest(t, http.MethodPost, base+"/api/v1/sessions", "fixed-token", push); status != http.StatusOK || !strings.Contains(body, `"stored":1`) {
		t.Fatalf("带令牌推送 = %d %s", status, body)
	}
	if body, status := serveRequest(t, http.MethodGet, base+"/api/v1/sessions?host=node-test", "fixed-token", ""); status != http.StatusOK || !strings.Contains(body, "node-test") {
		t.Errorf("列表 = %d %s", status, body)
	}
	for _, want := range []string{"记录库：", "监听：  http://127.0.0.1:", "端点凭据：", "客户端：assistant session push"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("启动输出缺少 %q：\n%s", want, out.String())
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("取消后应正常停止：%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("取消上下文后 runServe 未退出（优雅停机分支失效）")
	}
	if !strings.Contains(out.String(), "已停止监听") {
		t.Errorf("输出应说明已停止：\n%s", out.String())
	}
	if _, err := os.Stat(endpointPath); !os.IsNotExist(err) {
		t.Errorf("端点文件应随退出删除（否则客户端会连到死端口）：%v", err)
	}
}

// TestRunServeReportsListenFailure 断言监听地址被占用时上抛并点名地址：
// serve 是常驻进程，静默启动失败会让操作者以为服务在跑，而实际上没有任何端口
// 在工作——错误必须带上地址，否则多实例场景下不知道该杀掉谁。
func TestRunServeReportsListenFailure(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	// 先占住一个端口，再让 runServe 去监听同一个地址。
	listener, err := netListen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())

	err = runServe(command, configPath, &serveOptions{Listen: address, Root: filepath.Join(t.TempDir(), "s"), Token: "t"})
	if err == nil {
		t.Fatal("端口被占用时应报错")
	}
	if !strings.Contains(err.Error(), address) || !strings.Contains(err.Error(), "监听") {
		t.Errorf("err = %v, want 含「监听 <地址>」", err)
	}
}

// TestRunServeRejectsDirectoryWithoutManifest 断言记录库目录不合规时提前失败：
// 记录库根必须有 manifest.json，否则 serve 会把别人的数据当成自己的库写进去
// （或反过来把自己的 meta 混进无关目录）。--force 之外必须拒绝，且拒绝时不得
// 留下监听中的端口。
func TestRunServeRejectsDirectoryWithoutManifest(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "someone-else.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())

	err := runServe(command, configPath, &serveOptions{Listen: "127.0.0.1:0", Root: foreign, Token: "t"})
	if err == nil {
		t.Fatal("非空且无 manifest 的目录应被拒绝")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("err = %v, want 提到 manifest", err)
	}
	if _, statErr := os.Stat(filepath.Join(foreign, sessionstore.ServeFile)); !os.IsNotExist(statErr) {
		t.Error("拒绝时不应写出端点文件（调用方会误以为服务已就绪）")
	}
}

// TestListFilterReadsEveryQueryFieldAndDropsBadSince 断言过滤条件的解析口径：
// 每个查询参数都要映射到对应字段（漏一个会让检索静默返回全库结果），since
// 解析失败时必须退化成「不过滤时间」而不是报错或置零时间——置零会把所有记录
// 都排除掉，操作者会以为索引丢了。
func TestListFilterReadsEveryQueryFieldAndDropsBadSince(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet,
		"/api/v1/sessions?host=h&project=p&session=s&conversation=c&source=review&since=2026-09-01T10:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	filter := listFilter(request)
	if filter.Host != "h" || filter.Project != "p" || filter.Session != "s" ||
		filter.Conversation != "c" || filter.Source != "review" {
		t.Errorf("filter = %+v，字段未全部映射", filter)
	}
	want, err := time.Parse(time.RFC3339, "2026-09-01T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !filter.Since.Equal(want) {
		t.Errorf("Since = %v, want %v", filter.Since, want)
	}

	// 空白参数被视为未提供（而不是空字符串过滤条件）。
	blank, err := http.NewRequest(http.MethodGet, "/api/v1/sessions?host=%20%20&source=", nil)
	if err != nil {
		t.Fatal(err)
	}
	if filter := listFilter(blank); filter.Host != "" || filter.Source != "" {
		t.Errorf("空白参数应视为未提供：%+v", filter)
	}

	// since 坏掉：退化成零值时间（= 不过滤），不能报错也不能排掉全部记录。
	broken, err := http.NewRequest(http.MethodGet, "/api/v1/sessions?since=yesterday", nil)
	if err != nil {
		t.Fatal(err)
	}
	if filter := listFilter(broken); !filter.Since.IsZero() {
		t.Errorf("坏的 since 应退化为零值时间，got %v", filter.Since)
	}
}

// TestIntQueryFallsBackOnMissingAndBadValues 断言分页参数的容错口径：缺省与
// 非法值都退回 fallback（非法值若被当成 0，limit=0 与 limit=abc 会不可区分，
// 而 0 在列表接口里是「不限条数」——一个笔误会把整库倒出来）。
func TestIntQueryFallsBackOnMissingAndBadValues(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		fallback int
		want     int
	}{
		{"缺省", "", 50, 50},
		{"合法值", "?limit=7", 50, 7},
		{"零是合法值", "?limit=0", 50, 0},
		{"负数原样返回", "?limit=-3", 50, -3},
		{"非数字退回缺省", "?limit=abc", 50, 50},
		{"空白退回缺省", "?limit=%20%20", 50, 50},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, "/api/v1/sessions"+testCase.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := intQuery(request, "limit", testCase.fallback); got != testCase.want {
				t.Errorf("intQuery(%q) = %d, want %d", testCase.query, got, testCase.want)
			}
		})
	}
}

// TestWriteServeEndpointCreatesDirWithTightMode 断言端点文件的落盘约定：父目录
// 不存在要自建（--config 指向的新目录是常见情形），文件权限必须是 0600——文件里
// 有访问令牌，宽松权限等于把记录库公开给本机所有用户。
func TestWriteServeEndpointCreatesDirWithTightMode(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "cfg", "nested")
	path, err := writeServeEndpoint(configDir, serveEndpoint{URL: "http://127.0.0.1:8780", Token: "tok"})
	if err != nil {
		t.Fatalf("writeServeEndpoint: %v", err)
	}
	if filepath.Dir(path) != configDir {
		t.Errorf("路径 = %q, want 落在 %q", path, configDir)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("端点文件权限 = %#o, want 0600（内含访问令牌）", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || !bytes.HasSuffix(data, []byte("\n")) {
		t.Errorf("端点文件应是合法 JSON 且以换行结尾：%q", data)
	}

	// 父目录是个文件：必须上抛而不是静默失败。
	blocked := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeServeEndpoint(blocked, serveEndpoint{}); err == nil {
		t.Error("配置目录被普通文件占用时应报错")
	}
}

// TestServeHandlerRejectsBadRequests 断言接口的拒绝路径：请求体坏掉、路径段数
// 不对、记录不存在、方法不支持，都要给出对应的状态码与可读原因。这些分支是
// 客户端排错的唯一线索；静默 200 会让 push 以为写成功了。
func TestServeHandlerRejectsBadRequests(t *testing.T) {
	store, err := sessionstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newLocalServer(t, serveHandler(store, "tok", func(string, ...any) {}))

	if body, status := serveRequest(t, http.MethodPost, server+"/api/v1/sessions", "tok", "{不是 JSON"); status != http.StatusBadRequest || !strings.Contains(body, "解析请求体失败") {
		t.Errorf("坏请求体 = %d %s, want 400 且说明解析失败", status, body)
	}
	if _, status := serveRequest(t, http.MethodPut, server+"/api/v1/sessions", "tok", ""); status != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/v1/sessions = %d, want 405", status)
	}
	// 推送带非法键的记录：store.Put 失败要变成 400，而不是 500。
	badKey := `{"sessions":[{"host":"../evil","project":"p","session":"s","pushed_at":"2026-09-26T10:00:00Z","lines":["{}"]}]}`
	if _, status := serveRequest(t, http.MethodPost, server+"/api/v1/sessions", "tok", badKey); status != http.StatusBadRequest {
		t.Errorf("非法键 = %d, want 400", status)
	}
	// 路径段数不对：400 且给出正确形如。响应经过 encoding/json 转义，尖括号是
	// </>，断言要按转义后的形态匹配。
	if body, status := serveRequest(t, http.MethodGet, server+"/api/v1/sessions/only/two", "tok", ""); status != http.StatusBadRequest || !strings.Contains(body, `\u003chost\u003e/\u003cproject\u003e/\u003csession\u003e`) {
		t.Errorf("路径段数不对 = %d %s, want 400 并给出正确形如", status, body)
	}
	if _, status := serveRequest(t, http.MethodGet, server+"/api/v1/sessions/nohost/noproject/nosession", "tok", ""); status != http.StatusNotFound {
		t.Errorf("不存在的记录 = %d, want 404", status)
	}
	// 空库的列表/检索必须回 [] 而不是 null：客户端按数组解析，null 会直接崩。
	if body, status := serveRequest(t, http.MethodGet, server+"/api/v1/sessions", "tok", ""); status != http.StatusOK || !strings.Contains(body, `"sessions":[]`) {
		t.Errorf("空库列表 = %d %s, want 空数组", status, body)
	}
	if body, status := serveRequest(t, http.MethodGet, server+"/api/v1/search?q=x", "tok", ""); status != http.StatusOK || !strings.Contains(body, `"matches":[]`) {
		t.Errorf("空库检索 = %d %s, want 空数组", status, body)
	}
	// 令牌写错（含前缀不匹配）一律 401。
	if _, status := serveRequest(t, http.MethodGet, server+"/api/v1/conversations", "wrong", ""); status != http.StatusUnauthorized {
		t.Errorf("错误令牌 = %d, want 401", status)
	}
	if body, status := serveRequest(t, http.MethodGet, server+"/api/v1/conversations", "tok", ""); status != http.StatusOK || !strings.Contains(body, `"conversations":`) {
		t.Errorf("会话实体列表 = %d %s", status, body)
	}
}

// ---- 夹具 ----

// takeServeListenAddress 读取上一次 runServe 写下的真实监听地址（serve.go 里的
// 包级观测点，生产逻辑不依赖它）。
func takeServeListenAddress(t *testing.T) string {
	t.Helper()
	if serveListenAddress == "" {
		t.Fatal("runServe 未回填真实监听地址（夹具失效）")
	}
	return serveListenAddress
}

// waitForServeEndpoint 轮询端点文件直到它出现，返回解析结果。runServe 在
// 监听成功后才写这个文件，所以它的出现是「服务已就绪」的信号，比固定 sleep
// 更可靠（慢机器上 sleep 会假失败，快机器上白等）。
func waitForServeEndpoint(t *testing.T, path string) serveEndpoint {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			var endpoint serveEndpoint
			if err := json.Unmarshal(data, &endpoint); err == nil && endpoint.URL != "" {
				return endpoint
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待端点文件 %s 超时（serve 没起来）", path)
	return serveEndpoint{}
}

// serveRequest 发一次请求并回读响应体，供断言状态码与内容。
func serveRequest(t *testing.T, method, url, token, body string) (string, int) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), response.StatusCode
}

// newLocalServer 起一个只服务本次测试的假服务端（供 serveHandler 的拒绝路径
// 逐个断言），返回其 URL。
func newLocalServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// netListen 是 net.Listen 的薄封装，仅为了让测试夹具不必直接 import net。
func netListen(address string) (net.Listener, error) { return net.Listen("tcp", address) }
