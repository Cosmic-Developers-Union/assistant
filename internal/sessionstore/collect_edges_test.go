package sessionstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Collect 的边界：非目录条目被跳过、非 .jsonl 文件被跳过、读不到的项目目录被跳过、
// chat 目录里的 session.json 决定来源与映射、坏档被跳过。
func TestCollectBoundaries(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude")
	projectsDir := filepath.Join(root, "projects")
	// 一个真项目目录
	projectDir := filepath.Join(projectsDir, "-tmp-work")
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "s-1.jsonl"), sampleTranscript); err != nil {
		t.Fatal(err)
	}
	// 一个不是 .jsonl 的文件（不该被当记录）
	if err := writeFile(filepath.Join(projectDir, "notes.txt"), "忽略我"); err != nil {
		t.Fatal(err)
	}
	// projects 下直接放一个普通文件（不是目录，应被跳过）
	if err := writeFile(filepath.Join(projectsDir, "stray.txt"), "忽略我"); err != nil {
		t.Fatal(err)
	}
	// 一个子目录（在项目目录里，应被跳过）
	if err := osMkdirAll(filepath.Join(projectDir, "nested.jsonl")); err != nil {
		t.Fatal(err)
	}

	// chat 目录：三个会话目录——正常、缺 session_id、坏 JSON
	chatDir := filepath.Join(dir, "chat")
	for name, content := range map[string]string{
		"good":    `{"conversation_id":"c-1","session_id":"s-1","title":"t","model":"m","transport":"weixin"}`,
		"noid":    `{"conversation_id":"c-2","title":"没有会话 id"}`,
		"broken":  `{`,
		"missing": "",
	} {
		entryDir := filepath.Join(chatDir, name)
		if err := osMkdirAll(entryDir); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if err := writeFile(filepath.Join(entryDir, ChatSessionFile), content); err != nil {
				t.Fatal(err)
			}
		}
	}
	// chat 目录下的普通文件也应被跳过
	if err := writeFile(filepath.Join(chatDir, "stray.json"), "[]"); err != nil {
		t.Fatal(err)
	}

	batch, err := Collect(CollectOptions{Root: root, Host: "node-1", ChatDir: chatDir})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(batch.Sessions) != 1 {
		t.Fatalf("应只采到 1 条：%+v", batch.Sessions)
	}
	session := batch.Sessions[0]
	if session.Key.String() != "node-1/-tmp-work/s-1" {
		t.Errorf("会话键 = %s", session.Key.String())
	}
	if session.Source != "chat" || session.Conversation != "c-1" || session.Title != "t" || session.Model != "m" {
		t.Errorf("chat 映射未带上：%+v", session.Meta)
	}
	if session.CreatedAt == "" || session.UpdatedAt != session.CreatedAt {
		t.Errorf("时间戳 = %q/%q", session.CreatedAt, session.UpdatedAt)
	}

	// projects 目录不存在：空结果不算错（干净机器上的正常情形）
	if batch, err := Collect(CollectOptions{Root: filepath.Join(dir, "empty")}); err != nil || len(batch.Sessions) != 0 {
		t.Errorf("缺 projects 目录应为空结果：%+v err=%v", batch, err)
	}
	// 根目录是普通文件：读 projects 失败应报错（而不是当空目录）
	blockedRoot := filepath.Join(dir, "root-file")
	if err := writeFile(blockedRoot, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(CollectOptions{Root: blockedRoot}); err == nil {
		t.Error("根目录是普通文件时应报错")
	}
}

// readChatSessions 的缺省 transport 是 weixin（旧版工作目录没写 transport 字段）。
func TestReadChatSessionsDefaultsTransport(t *testing.T) {
	chatDir := t.TempDir()
	entryDir := filepath.Join(chatDir, "legacy")
	if err := osMkdirAll(entryDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(entryDir, ChatSessionFile), `{"session_id":"s-legacy","conversation_id":"c-1"}`); err != nil {
		t.Fatal(err)
	}
	sessions := readChatSessions(chatDir)
	if sessions["s-legacy"].Transport != "weixin" {
		t.Errorf("缺 transport 应回落 weixin：%+v", sessions)
	}
	// 空目录 / 不存在的目录：空索引，不报错
	if len(readChatSessions("")) != 0 || len(readChatSessions(filepath.Join(chatDir, "absent"))) != 0 {
		t.Error("空/缺失 chat 目录应返回空索引")
	}
}

// SessionFor 的边界：空会话 id 直接 ok=false、缺 projects 目录 ok=false、
// 项目目录里读不到文件时返回错误（不吞）。
func TestSessionForBoundaries(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude")
	projectDir := filepath.Join(root, "projects", "-tmp-work")
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "s-1.jsonl"), sampleTranscript); err != nil {
		t.Fatal(err)
	}
	options := CollectOptions{Root: root, Host: "n", ChatDir: filepath.Join(dir, "chat")}

	// 空会话 id：ok=false，且不报错（调用方不必先判断）
	if _, ok, err := SessionFor(options, "   "); ok || err != nil {
		t.Errorf("空会话 id = ok=%v err=%v", ok, err)
	}
	// 缺 projects 目录：ok=false，不报错
	if _, ok, err := SessionFor(CollectOptions{Root: filepath.Join(dir, "empty")}, "s-1"); ok || err != nil {
		t.Errorf("缺 projects 目录 = ok=%v err=%v", ok, err)
	}
	// 命中：读到记录与元数据
	session, ok, err := SessionFor(options, "s-1")
	if err != nil || !ok || session.Key.String() != "n/-tmp-work/s-1" {
		t.Fatalf("SessionFor = %+v ok=%v err=%v", session.Meta, ok, err)
	}
	if session.FirstUserText == "" {
		t.Error("摘要应为空以外的值")
	}
	// 命中但不是 chat 会话：来源由项目名推断
	if session.Source != "unknown" {
		t.Errorf("来源 = %q（项目名 -tmp-work 应对应 unknown）", session.Source)
	}
}

// Client 各查询方法的过滤条件序列化，以及坏响应/非 2xx 的错误文案。
func TestClientQueryAndErrors(t *testing.T) {
	// filterQuery：零值不出参数，给值就出对应键
	if values := filterQuery(Filter{}); len(values) != 0 {
		t.Errorf("零值过滤 = %v", values)
	}
	since := time.Date(2026, 9, 15, 11, 31, 4, 0, time.UTC)
	values := filterQuery(Filter{Host: "h", Project: "p", Session: "s", Conversation: "c", Source: "chat", Since: since})
	for key, want := range map[string]string{
		"host": "h", "project": "p", "session": "s", "conversation": "c", "source": "chat",
		"since": "2026-09-15T11:31:04Z",
	} {
		if values.Get(key) != want {
			t.Errorf("filterQuery[%s] = %q, want %q", key, values.Get(key), want)
		}
	}
	// Since 带非 UTC 时区：序列化前转 UTC
	location := time.FixedZone("CST", 8*3600)
	local := time.Date(2026, 9, 15, 19, 31, 4, 0, location)
	if got := filterQuery(Filter{Since: local}).Get("since"); got != "2026-09-15T11:31:04Z" {
		t.Errorf("since 未转 UTC：%q", got)
	}

	// 空 BaseURL：本地校验报错，不发请求
	if _, err := NewClient("", "").List(t.Context(), Filter{}, 10); err == nil || !strings.Contains(err.Error(), "没有配置记录库服务端地址") {
		t.Errorf("空地址应报错：%v", err)
	}
	// BaseURL 末尾斜杠被去掉
	if client := NewClient(" http://x.example/ ", "  t  "); client.BaseURL != "http://x.example" || client.Token != "t" {
		t.Errorf("NewClient 未规范化：%+v", client)
	}

	// 坏响应（不是 JSON）→ 解析错误
	hostile := fakeServerRaw(t, `不是 JSON`, 200)
	if _, err := NewClient(hostile.URL, "").Conversations(t.Context()); err == nil || !strings.Contains(err.Error(), "解析") {
		t.Errorf("坏响应应报解析错误：%v", err)
	}
	// 非 2xx → 带状态码与响应体
	failing := fakeServerRaw(t, `服务端炸了`, 500)
	if _, err := NewClient(failing.URL, "").List(t.Context(), Filter{}, 10); err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "服务端炸了") {
		t.Errorf("非 2xx 文案不对：%v", err)
	}
	// 404 读不存在的记录
	if _, err := NewClient(failing.URL, "").Read(t.Context(), Key{Host: "h", Project: "p", Session: "s"}, 0, 0); err == nil {
		t.Error("404 应报错")
	}

	// 带 token 时请求头带上 Bearer（服务端校验）
	authServer := fakeServerAuth(t, "secret-token")
	if _, err := NewClient(authServer.URL, "secret-token").Conversations(t.Context()); err != nil {
		t.Errorf("带正确 token 应通过：%v", err)
	}
	if _, err := NewClient(authServer.URL, "wrong").Conversations(t.Context()); err == nil {
		t.Error("错误 token 应被拒绝")
	}
}

// Push 的客户端层：服务端返回非 2xx 时错误带上服务端信息。
func TestClientPushError(t *testing.T) {
	failing := fakeServerRaw(t, `{"message":"库坏了"}`, 500)
	if _, err := NewClient(failing.URL, "").Push(t.Context(), Batch{Sessions: []Session{{Meta: Meta{Key: Key{Host: "h", Project: "p", Session: "s"}}}}}); err == nil {
		t.Error("非 2xx 推送应报错")
	}
}

// 采集/推送的端到端往返：Collect → Push → 服务端 Store 里真的落了记录。
func TestCollectPushRoundTrip(t *testing.T) {
	options := pushOptions(t)
	serverStore, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	server := fakeServer(t, serverStore)
	result, err := Push(t.Context(), options, NewClient(server.URL, ""), func(format string, args ...any) {
		if !strings.Contains(format, "已推送") {
			t.Errorf("日志格式 = %q", format)
		}
	})
	if err != nil || result.Stored != 1 {
		t.Fatalf("Push = %+v err=%v", result, err)
	}
	metas, err := serverStore.List(Filter{}, 0)
	if err != nil || len(metas) != 1 || metas[0].Key.String() != "node-1/-tmp-p/s-1" {
		t.Fatalf("服务端未落库：%+v err=%v", metas, err)
	}
	messages, err := serverStore.Read(metas[0].Key, 0, 0)
	if err != nil || len(messages) == 0 {
		t.Fatalf("服务端记录不可读：%+v err=%v", messages, err)
	}
}

// fakeServerRaw 返回固定响应体的最小服务端（测客户端错误路径用）。
func fakeServerRaw(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// fakeServerAuth 校验 Authorization 头（测 token 透传）。
func fakeServerAuth(t *testing.T, token string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{"conversations": map[string]int{"c-1": 1}}); err != nil {
			t.Errorf("编码响应: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
