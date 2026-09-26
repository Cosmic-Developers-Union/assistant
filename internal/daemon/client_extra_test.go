package daemon

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

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// writeEndpointFile 在 path 写一份端点 JSON（供 Discover 的成功/缺字段分支用）。
func writeEndpointFile(t *testing.T, path string, payload string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverPathOverrideAndErrors 钉住 Discover 的四类失败语义：找不到端点
// 文件（daemon 没在跑）、文件读不出来、JSON 坏掉、以及 addr/token 缺字段。
// 这些都是「MCP 工具调不通」时操作者唯一能看到的线索，错误文案必须点名文件
// 路径与具体原因，否则无从排查。
func TestDiscoverPathOverrideAndErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()

	// 1) ASSISTANT_DAEMON_ENDPOINT 覆盖路径：文件不存在 → 「先 assistant run」
	missing := filepath.Join(dir, "nope.json")
	getenv := func(key string) string {
		if key == EndpointEnv {
			return missing
		}
		return ""
	}
	if _, err := Discover(getenv); err == nil || !strings.Contains(err.Error(), "先 assistant run") {
		t.Errorf("端点文件缺失时错误应提示先 assistant run：%v", err)
	}

	// 2) 路径是目录 → 读取失败走「读取端点文件」分支（不是 IsNotExist）
	asDir := filepath.Join(dir, "as-dir")
	if err := os.MkdirAll(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(func(key string) string {
		if key == EndpointEnv {
			return asDir
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "读取端点文件") {
		t.Errorf("读取失败应报「读取端点文件」：%v", err)
	}

	// 3) JSON 坏掉 → 「解析端点文件」
	broken := filepath.Join(dir, "broken.json")
	writeEndpointFile(t, broken, "{not json")
	if _, err := Discover(func(key string) string {
		if key == EndpointEnv {
			return broken
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "解析端点文件") {
		t.Errorf("JSON 坏掉应报「解析端点文件」：%v", err)
	}

	// 4) 缺 token → 提示 daemon 可能已退出（写了 addr 但没 token 最常见）
	partial := filepath.Join(dir, "partial.json")
	writeEndpointFile(t, partial, `{"addr":"127.0.0.1:1"}`)
	if _, err := Discover(func(key string) string {
		if key == EndpointEnv {
			return partial
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "缺少 addr/token") {
		t.Errorf("缺 token 应报「缺少 addr/token」：%v", err)
	}

	// 5) 正常文件 + ASSISTANT_DAEMON_ADDR 覆盖地址：地址必须被覆盖成新值，
	// token 保留文件里的（这是「端口会被 daemon 重写」的调试开关）。
	good := filepath.Join(dir, "good.json")
	writeEndpointFile(t, good, `{"addr":"10.0.0.1:9","token":"tok-from-file"}`)
	client, err := Discover(func(key string) string {
		switch key {
		case EndpointEnv:
			return good
		case EndpointAddrEnv:
			return "127.0.0.1:2"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := client.Endpoint(); got.Addr != "127.0.0.1:2" || got.Token != "tok-from-file" {
		t.Errorf("地址覆盖后端点 = %+v（addr 应被覆盖，token 保留）", got)
	}
}

// TestDiscoverDefaultPathFromConfigDir 钉住 getenv 为 nil 与未设环境变量时的
// 回落路径：走 instances.DaemonEndpointPath()——ASSISTANT_CONFIG 指向别处时
// 端点文件跟着配置走。否则从任意目录启动的 MCP 找不到 daemon。
func TestDiscoverDefaultPathFromConfigDir(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(configDir, "config.json"))
	t.Setenv(EndpointEnv, "")
	t.Setenv(EndpointAddrEnv, "")

	// nil getenv → 必须用 os.Getenv（会读到上面清空的环境变量）
	if _, err := Discover(nil); err == nil || !strings.Contains(err.Error(), "先 assistant run") {
		t.Fatalf("未运行时 nil getenv 应报未运行：%v", err)
	}

	path, err := instances.DaemonEndpointPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, configDir) {
		t.Fatalf("端点路径应跟随 ASSISTANT_CONFIG 落在 %s 下，实际 %s", configDir, path)
	}
	writeEndpointFile(t, path, `{"addr":"127.0.0.1:3","token":"tok-default","version":"v-x"}`)
	client, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover 默认路径：%v", err)
	}
	if got := client.Endpoint(); got.Addr != "127.0.0.1:3" || got.Version != "v-x" {
		t.Errorf("端点 = %+v", got)
	}
}

// TestClientMissingEnvVarFallsBackToDefaultPath 钉住空串环境变量不等于「覆盖
// 成空」：Setenv 设成空串时必须回落标准路径，否则 shell 里 `ASSISTANT_DAEMON_
// ENDPOINT=` 会让人发现不了 daemon。
func TestClientMissingEnvVarFallsBackToDefaultPath(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(configDir, "config.json"))
	t.Setenv(EndpointEnv, "")
	path, err := instances.DaemonEndpointPath()
	if err != nil {
		t.Fatal(err)
	}
	writeEndpointFile(t, path, `{"addr":"127.0.0.1:4","token":"tok"}`)
	client, err := Discover(os.Getenv)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if client.endpoint.Addr != "127.0.0.1:4" {
		t.Errorf("空环境变量应回落标准路径：%+v", client.endpoint)
	}
}

// TestClientAccessorsAndQueue 钉住 Queue/Endpoint 两个此前零覆盖的访问器：
// 它们是 MCP list_queue 工具的取数路径，返回空串或错端点会让工具永远空响应。
func TestClientAccessorsAndQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/queue" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer tok-q" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode([]Queue{{Host: "https://gitea.example.com", Repository: "acme/repo"}})
	}))
	defer server.Close()

	client := NewClient(Endpoint{Addr: strings.TrimPrefix(server.URL, "http://"), Token: "tok-q"})
	if got := client.Endpoint().Token; got != "tok-q" {
		t.Errorf("Endpoint().Token = %q", got)
	}
	queue, err := client.Queue(t.Context())
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 1 || queue[0].Repository != "acme/repo" {
		t.Errorf("Queue = %+v", queue)
	}
}

// TestClientGetNonOKStatus 钉住 get 的非 200 分支：错误里必须带服务端返回的
// 正文（截断），否则「401 invalid token」这类关键信息会被吞掉，只剩一个状态码。
func TestClientGetNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":"invalid token"}` + "\n"))
	}))
	defer server.Close()

	client := NewClient(Endpoint{Addr: strings.TrimPrefix(server.URL, "http://"), Token: "bad"})
	_, err := client.Results(t.Context(), 5)
	if err == nil {
		t.Fatal("非 200 应报错")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("错误应带状态码与正文：%v", err)
	}
}

// TestClientRequestErrorOnUnreachable 钉住网络层失败（连不上的地址）：错误要
// 带上目标地址，方便判断是 daemon 挂了还是配置写错了。
func TestClientRequestErrorOnUnreachable(t *testing.T) {
	client := NewClient(Endpoint{Addr: "127.0.0.1:1", Token: "tok"})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := client.Status(ctx); err == nil || !strings.Contains(err.Error(), "请求 daemon") {
		t.Errorf("连不上应报「请求 daemon」：%v", err)
	}
}
