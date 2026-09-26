package sessionstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Open 已存在且带正确 manifest 时直接复用身份（不重写、不覆盖），并用 manifest 里的
// host 作为缺省宿主机标签。
func TestStoreOpenReusesManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	first, err := Open(root, OpenOptions{Host: "node-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("manifest 不是 JSON：%v", err)
	}
	for _, field := range []string{"format", "version", "layout", "host", "created_at", "app"} {
		if _, ok := document[field]; !ok {
			t.Errorf("manifest 缺字段 %q：%s", field, raw)
		}
	}
	if document["format"] != Format || document["version"] != float64(FormatVersion) {
		t.Errorf("manifest 身份 = %v/%v", document["format"], document["version"])
	}
	if document["host"] != "node-1" || first.Manifest().Host != "node-1" {
		t.Errorf("manifest host = %v / %q", document["host"], first.Manifest().Host)
	}

	// 二次 Open：身份不变（created_at 不被改写，说明没重建 manifest）
	second, err := Open(root, OpenOptions{Host: "someone-else"})
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	if second.Root() != root || second.Manifest().Host != "node-1" || second.Manifest().CreatedAt != first.Manifest().CreatedAt {
		t.Errorf("二次 Open 改写了身份：%+v", second.Manifest())
	}

	// 别人家的 manifest（format 不符）：报错并带上对方的格式名
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err := osMkdirAll(foreign); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(foreign, ManifestFile), `{"format":"other.tool","version":9}`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(foreign, OpenOptions{}); err == nil || !strings.Contains(err.Error(), "other.tool") {
		t.Errorf("外来 manifest 应报错并说明格式：%v", err)
	}

	// 非空目录缺 manifest：默认拒绝（--force 才接管）
	occupied := filepath.Join(t.TempDir(), "occupied")
	if err := osMkdirAll(occupied); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(occupied, "someone.txt"), "别人的东西"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(occupied, OpenOptions{}); err == nil {
		t.Error("非空目录缺 manifest 应被拒绝")
	}
	forced, err := Open(occupied, OpenOptions{Force: true})
	if err != nil {
		t.Fatalf("--force 应能接管：%v", err)
	}
	if forced.Manifest().Format != Format {
		t.Errorf("接管后 manifest 不对：%+v", forced.Manifest())
	}
}

// 空标签的 host 兜底用机器名；不传 Host 也应该写出一份可用的 manifest。
func TestStoreOpenFallsBackHost(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if strings.TrimSpace(store.Manifest().Host) == "" {
		t.Error("缺省 host 不该为空（应回落到机器名）")
	}
}

// Put：非法键被拒绝、目录自动创建、UpdatedAt 缺省用落库时间、Lines/Bytes 由内容算出。
func TestStorePutBoundaries(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Put(Session{Meta: Meta{Key: Key{Host: "h", Project: "p"}}}); err == nil {
		t.Error("非法键应被拒绝")
	}

	// 落库：目录不存在也自动创建
	key := Key{Host: "h", Project: "deep/p", Session: "s"}
	if _, err := store.Put(Session{Meta: Meta{Key: key}, Lines: []string{"x\n\n", "y"}}); err == nil {
		t.Fatal("含斜杠的项目名应被拒绝（防目录穿越）")
	}

	key = Key{Host: "h", Project: "p", Session: "s"}
	meta, err := store.Put(Session{Meta: Meta{Key: key}, Lines: []string{"x\n\n", "y"}})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Lines != 2 || meta.Bytes != 4 {
		t.Errorf("Lines/Bytes = %d/%d，应为 2/4", meta.Lines, meta.Bytes)
	}
	if meta.UpdatedAt == "" || meta.UpdatedAt != meta.PushedAt {
		t.Errorf("UpdatedAt 应缺省等于 PushedAt：%+v", meta)
	}
	// 落盘的 jsonl 每行只留一个 \n（行尾被规范化）
	data, err := os.ReadFile(filepath.Join(store.Root(), "h", "p", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "x\ny\n" {
		t.Errorf("jsonl = %q", data)
	}

	// 显式 UpdatedAt 不被覆盖
	explicit, err := store.Put(Session{Meta: Meta{Key: Key{Host: "h", Project: "p", Session: "s2"}, UpdatedAt: "2020-01-01T00:00:00Z"}, Lines: []string{"z"}})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if explicit.UpdatedAt != "2020-01-01T00:00:00Z" {
		t.Errorf("显式 UpdatedAt 被覆盖：%+v", explicit)
	}
}

// List 的排序与过滤：更新时间倒序，同一时间按会话键升序（稳定、可复现）；
// limit<=0 表示不限；Since 过滤走 Meta.UpdatedAt。
func TestStoreListOrderingAndFilter(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// 两条同一时间（按 Key 升序）、一条更新
	for _, spec := range []struct{ session, updated string }{
		{"b", "2026-09-15T00:00:00Z"},
		{"a", "2026-09-15T00:00:00Z"},
		{"c", "2026-09-16T00:00:00Z"},
	} {
		if _, err := store.Put(Session{Meta: Meta{Key: Key{Host: "h", Project: "p", Session: spec.session}, UpdatedAt: spec.updated, Conversation: "c-1"}, Lines: []string{"{bad json}"}}); err != nil {
			t.Fatalf("Put(%s): %v", spec.session, err)
		}
	}
	metas, err := store.List(Filter{}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, 0, len(metas))
	for _, meta := range metas {
		got = append(got, meta.Session)
	}
	if strings.Join(got, ",") != "c,a,b" {
		t.Errorf("排序 = %v，应为 c,a,b（时间倒序 + 同时间键升序）", got)
	}

	// limit 生效
	if limited, err := store.List(Filter{}, 2); err != nil || len(limited) != 2 {
		t.Fatalf("limit = %+v err=%v", limited, err)
	}

	// 会话/项目/来源过滤
	if filtered, err := store.List(Filter{Session: "a"}, 0); err != nil || len(filtered) != 1 || filtered[0].Session != "a" {
		t.Fatalf("按会话过滤 = %+v err=%v", filtered, err)
	}
	if filtered, err := store.List(Filter{Project: "other"}, 0); err != nil || len(filtered) != 0 {
		t.Fatalf("按项目过滤 = %+v err=%v", filtered, err)
	}
	since := mustParse(t, "2026-09-16T00:00:00Z")
	if filtered, err := store.List(Filter{Since: since}, 0); err != nil || len(filtered) != 1 || filtered[0].Session != "c" {
		t.Fatalf("按时间下界过滤 = %+v err=%v", filtered, err)
	}

	// 会话实体计数
	counts, err := store.Conversations()
	if err != nil || counts["c-1"] != 3 {
		t.Fatalf("Conversations = %v err=%v", counts, err)
	}
	// 空会话实体的记录不进计数
	if _, err := store.Put(Session{Meta: Meta{Key: Key{Host: "h", Project: "p", Session: "no-conv"}}, Lines: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.Conversations(); err != nil || len(counts) != 1 {
		t.Fatalf("空会话实体应被跳过 = %v err=%v", counts, err)
	}

	// 检索空词报错
	if _, err := store.Search("   ", Filter{}, 0); err == nil || !strings.Contains(err.Error(), "检索词为空") {
		t.Errorf("空检索词应报错：%v", err)
	}
}

// Read 的边界：offset 越过全部消息返回空而非报错；limit<=0 不限。
func TestStoreReadOffsetPastEnd(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	key := Key{Host: "h", Project: "p", Session: "s"}
	if _, err := store.Put(Session{Meta: Meta{Key: key}, Lines: strings.Split(sampleTranscript, "\n")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	all, err := store.Read(key, 0, 0)
	if err != nil || len(all) == 0 {
		t.Fatalf("Read = %+v err=%v", all, err)
	}
	if past, err := store.Read(key, len(all)+5, 0); err != nil || len(past) != 0 {
		t.Errorf("offset 越界应返回空：%+v err=%v", past, err)
	}
}

// State.Save/LoadState 的边界：空路径是 no-op（不报错）、损坏状态当空（最多重推一次）、
// 落盘是 0600 且是原子替换（同目录 .tmp）。
func TestStateSaveLoadBoundaries(t *testing.T) {
	if err := (State{"a": {Size: 1}}).Save("  "); err != nil {
		t.Errorf("空白路径应 no-op：%v", err)
	}
	if state := LoadState(""); len(state) != 0 {
		t.Errorf("空白路径应返回空状态：%+v", state)
	}
	if state := LoadState(filepath.Join(t.TempDir(), "absent.json")); len(state) != 0 {
		t.Errorf("缺失文件应返回空状态：%+v", state)
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := writeFile(broken, "{"); err != nil {
		t.Fatal(err)
	}
	if state := LoadState(broken); len(state) != 0 {
		t.Errorf("损坏状态应返回空状态：%+v", state)
	}

	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := (State{"k": {Size: 7, ModTime: "m"}}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("状态文件权限 = %v，应为 0600", info.Mode().Perm())
	}
	// 临时文件不残留
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("应原子替换，不该留下 .tmp：%v", err)
	}
	if state := LoadState(path); state["k"].Size != 7 {
		t.Errorf("往返失败：%+v", state)
	}
}

// RunMCP 的协议边界：坏 JSON 行被忽略、notification 不回包、未知方法报错、
// tools/call 的参数错误与工具报错都走 isError、EOF 正常收尾。
func TestRunMCPProtocolEdges(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	server := fakeServer(t, store)
	lines := []string{
		`这不是 JSON`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"不是对象"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"session_search","arguments":{"query":""}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"session_search","arguments":"不是对象"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"session_search","arguments":{"query":"没有这句话"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"session_list","arguments":{"limit":"2"}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"conversation_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"no/such/method"}`,
	}
	var out bytes.Buffer
	if err := RunMCP(t.Context(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, MCPOptions{Client: NewClient(server.URL, ""), Version: "test"}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}

	// 逐行解析回包：坏行与 notification 都没有回包
	decoded := make(map[float64]map[string]any)
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("回包不是 JSON：%s", scanner.Text())
		}
		id, _ := response["id"].(float64)
		decoded[id] = response
	}
	if len(decoded) != 11 {
		t.Errorf("回包数量 = %d，应为 11（坏行与 notification 不回）", len(decoded))
	}
	for _, wantID := range []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		if _, ok := decoded[wantID]; !ok {
			t.Fatalf("缺 id=%v 的回包：%v", wantID, decoded)
		}
	}
	// initialize 带 serverInfo
	result, _ := decoded[1]["result"].(map[string]any)
	if result == nil {
		t.Fatalf("initialize 回包 = %+v", decoded[1])
	}
	serverInfo, _ := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "assistant-sessions" || serverInfo["version"] != "test" {
		t.Errorf("serverInfo = %+v", serverInfo)
	}
	// params 本身不是对象：协议层 error；参数语义错误（空 query）走工具 error
	if decoded[4]["error"] == nil {
		t.Errorf("params 解析失败应走 error：%+v", decoded[4])
	}
	if inner, _ := decoded[5]["result"].(map[string]any); inner == nil || inner["isError"] != true {
		t.Errorf("空 query 应走工具 error：%+v", decoded[5])
	}
	// 工具内部失败走 isError=true 的 result
	inner, _ := decoded[6]["result"].(map[string]any)
	if inner == nil || inner["isError"] != true {
		t.Errorf("工具失败应走 isError result：%+v", decoded[6])
	}
	// 未知工具走工具 error，未知方法走协议 error
	if inner, _ := decoded[10]["result"].(map[string]any); inner == nil || inner["isError"] != true {
		t.Errorf("未知工具应走工具 error：%+v", decoded[10])
	}
	if decoded[11]["error"] == nil {
		t.Errorf("未知方法应走协议 error：%+v", decoded[11])
	}
	// 空库的友好文案
	for _, id := range []float64{7, 8, 9} {
		result, _ := decoded[id]["result"].(map[string]any)
		if result == nil {
			t.Fatalf("id=%v 应有 result：%+v", id, decoded[id])
		}
		content, _ := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("id=%v content = %+v", id, content)
		}
		block, _ := content[0].(map[string]any)
		text, _ := block["text"].(string)
		if !strings.Contains(text, "没有") && !strings.Contains(text, "记录库里没有") {
			t.Errorf("id=%v 空库文案 = %q", id, text)
		}
	}
}

// RunMCP 未给 Client 时按 ConfigDir 自行解析远端配置：没有配置就去请求必然失败，
// 但协议层不 panic，工具回包带错误文案。
func TestRunMCPWithoutClientResolvesRemote(t *testing.T) {
	var out bytes.Buffer
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_list","arguments":{}}}` + "\n"
	if err := RunMCP(t.Context(), strings.NewReader(request), &out, MCPOptions{ConfigDir: t.TempDir()}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	if !strings.Contains(out.String(), "isError") {
		t.Errorf("无远端配置时应返回工具错误：%s", out.String())
	}
	// 环境变量指向一个真实服务端时能查到记录
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Put(Session{Meta: Meta{Key: Key{Host: "n", Project: "p", Session: "s"}, Source: "chat"}, Lines: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	server := fakeServer(t, store)
	t.Setenv("ASSISTANT_SESSIONS_URL", server.URL)
	t.Setenv("ASSISTANT_SESSIONS_TOKEN", "t")
	out.Reset()
	if err := RunMCP(t.Context(), strings.NewReader(request), &out, MCPOptions{ConfigDir: t.TempDir()}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	if !strings.Contains(out.String(), "n/p/s") {
		t.Errorf("应查得到记录：%s", out.String())
	}
}

// 抽取工具的边角：toolResultText/compactJSON/snippet/DetectSource/intValue/
// conversationLabel/sortStrings 的退化输入。
func TestExtractHelperEdges(t *testing.T) {
	// toolResultText：字符串、内容块数组、空、坏档
	if got := toolResultText(json.RawMessage(`"直接文本"`)); got != "直接文本" {
		t.Errorf("toolResultText(string) = %q", got)
	}
	if got := toolResultText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":""},{"type":"text","text":"b"}]`)); got != "a b" {
		t.Errorf("toolResultText(blocks) = %q", got)
	}
	if got := toolResultText(json.RawMessage(`  `)); got != "" {
		t.Errorf("toolResultText(空) = %q", got)
	}
	if got := toolResultText(json.RawMessage(`123`)); got != "" {
		t.Errorf("toolResultText(不是文本) = %q", got)
	}

	// compactJSON：空、坏档、超长被截断加省略号
	if got := compactJSON(json.RawMessage(`   `)); got != "" {
		t.Errorf("compactJSON(空) = %q", got)
	}
	if got := compactJSON(json.RawMessage(`{`)); got != "" {
		t.Errorf("compactJSON(坏档) = %q", got)
	}
	long := `{"key":"` + strings.Repeat("字", 300) + `"}`
	if got := compactJSON(json.RawMessage(long)); !strings.HasSuffix(got, "…") || len([]rune(got)) != 201 {
		t.Errorf("compactJSON(超长) 长度 = %d，尾巴 = %q", len([]rune(got)), got[max(0, len(got)-10):])
	}

	// snippet：折行压成单空格、超长加省略号
	if got := snippet("a\n\n  b\tc", 100); got != "a b c" {
		t.Errorf("snippet = %q", got)
	}
	if got := snippet(strings.Repeat("字", 10), 4); got != "字字字字…" {
		t.Errorf("snippet(截断) = %q", got)
	}

	// DetectSource：chat 优先、assistant- 前缀是评审、空项目名是空、其它 unknown
	cases := map[string]string{
		"assistant-chat":               "chat",
		"assistant-node-1-acme-rocket": "review",
		"":                             "",
		"  ":                           "",
		"-tmp-somewhere":               "unknown",
	}
	for project, want := range cases {
		if got := DetectSource(project); got != want {
			t.Errorf("DetectSource(%q) = %q, want %q", project, got, want)
		}
	}

	// intValue：float64 / 数字字符串 / 不可解析回落
	if got := intValue(float64(7), 1); got != 7 {
		t.Errorf("intValue(float64) = %d", got)
	}
	if got := intValue(" 9 ", 1); got != 9 {
		t.Errorf("intValue(string) = %d", got)
	}
	if got := intValue("abc", 3); got != 3 {
		t.Errorf("intValue(坏字符串) = %d", got)
	}
	if got := intValue(nil, 5); got != 5 {
		t.Errorf("intValue(nil) = %d", got)
	}

	// conversationLabel / sortStrings
	if got := conversationLabel(""); got != "" {
		t.Errorf("conversationLabel(空) = %q", got)
	}
	if got := conversationLabel("c-1"); got != " 会话=c-1" {
		t.Errorf("conversationLabel = %q", got)
	}
	values := []string{"b", "a", "c", "a"}
	sortStrings(values)
	if strings.Join(values, ",") != "a,a,b,c" {
		t.Errorf("sortStrings = %v", values)
	}
}
