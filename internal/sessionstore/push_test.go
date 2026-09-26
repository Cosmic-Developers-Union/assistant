package sessionstore

import (
	"path/filepath"
	"strings"
	"testing"
)

// ResolveRemote 的解析次序：环境变量 > sessions-remote.json > serve.json；
// 环境变量只给一半时仍去文件里补另一半；坏档跳过而不是当空配置。
func TestResolveRemote(t *testing.T) {
	// 环境变量优先级最高：两个都给就短路，连文件都不用读
	t.Setenv("ASSISTANT_SESSIONS_URL", " https://env.example ")
	t.Setenv("ASSISTANT_SESSIONS_TOKEN", " env-token ")
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, RemoteFile), `{"url":"https://file.example","token":"file-token"}`); err != nil {
		t.Fatal(err)
	}
	config := ResolveRemote(dir)
	if config.URL != "https://env.example" || config.Token != "env-token" {
		t.Errorf("环境变量应优先且去空白：%+v", config)
	}

	// 只给 URL：URL 用环境变量，令牌从文件补
	t.Setenv("ASSISTANT_SESSIONS_TOKEN", "")
	config = ResolveRemote(dir)
	if config.URL != "https://env.example" || config.Token != "file-token" {
		t.Errorf("缺令牌时应从文件补：%+v", config)
	}

	// 环境变量全空：完全走文件
	t.Setenv("ASSISTANT_SESSIONS_URL", "")
	config = ResolveRemote(dir)
	if config.URL != "https://file.example" || config.Token != "file-token" {
		t.Errorf("应走 sessions-remote.json：%+v", config)
	}

	// 坏档（不是 JSON）跳过，回落到 serve.json
	if err := writeFile(filepath.Join(dir, RemoteFile), "{"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, ServeFile), `{"url":"https://serve.example","token":"serve-token"}`); err != nil {
		t.Fatal(err)
	}
	config = ResolveRemote(dir)
	if config.URL != "https://serve.example" || config.Token != "serve-token" {
		t.Errorf("坏档应回落到 serve.json：%+v", config)
	}

	// 两个文件都没有：返回零值（由调用方给出可读错误）
	config = ResolveRemote(t.TempDir())
	if config.URL != "" || config.Token != "" {
		t.Errorf("无配置应返回零值：%+v", config)
	}

	// 空配置目录：找文件失败不 panic，照样返回零值
	if config := ResolveRemote(""); config.URL != "" || config.Token != "" {
		t.Errorf("空配置目录应返回零值：%+v", config)
	}

	// 文件里的值带空白：读进来要去掉（否则拼出坏 URL）
	trimmed := t.TempDir()
	if err := writeFile(filepath.Join(trimmed, RemoteFile), `{"url":"  https://padded.example  ","token":"  padded-token  "}`); err != nil {
		t.Fatal(err)
	}
	if config := ResolveRemote(trimmed); config.URL != "https://padded.example" || config.Token != "padded-token" {
		t.Errorf("文件值应去空白：%+v", config)
	}
}

// pushOptions 造一份可推送的采集现场：一个 project 目录 + 一份 jsonl。
func pushOptions(t *testing.T) CollectOptions {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "claude")
	projectDir := filepath.Join(root, "projects", "-tmp-p")
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "s-1.jsonl"), sampleTranscript); err != nil {
		t.Fatal(err)
	}
	return CollectOptions{
		Root:      root,
		Host:      "node-1",
		ChatDir:   filepath.Join(dir, "chat"),
		StatePath: filepath.Join(dir, "session-push.json"),
	}
}

// Push：成功时逐条上传、写增量状态；失败时返回部分结果且不写状态
// （只记成功的那几条，下次重试补上）。
func TestPush(t *testing.T) {
	serverStore, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	server := fakeServer(t, serverStore)
	client := NewClient(server.URL, "")
	options := pushOptions(t)

	// 成功路径：nil logf 不该 panic（内部换成空函数），状态落盘
	result, err := Push(t.Context(), options, client, nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if result.Stored != 1 {
		t.Errorf("Stored = %d, want 1", result.Stored)
	}
	state := LoadState(options.StatePath)
	if len(state) != 1 {
		t.Fatalf("增量状态未写入：%+v", state)
	}
	entry, ok := state["node-1/-tmp-p/s-1"]
	if !ok || entry.Size == 0 || entry.ModTime == "" || entry.PushedAt == "" {
		t.Errorf("状态条目不完整：%+v", state)
	}
	// 状态写的是采集到的原文大小
	if want := int64(len(sampleTranscript)); entry.Size != want {
		t.Errorf("Size = %d, want %d", entry.Size, want)
	}

	// 第二次推送：增量判定生效，没有新记录可推，状态保持原样（不因 Stored==0 被清空）
	before := LoadState(options.StatePath)
	result, err = Push(t.Context(), options, client, func(string, ...any) {})
	if err != nil || result.Stored != 0 {
		t.Fatalf("增量推送 = %+v err=%v", result, err)
	}
	if after := LoadState(options.StatePath); len(after) != len(before) {
		t.Errorf("空推送不该改写状态：%+v", after)
	}

	// 推送失败：客户端指向一个必然报错的地址，返回部分结果 + 错误
	failing := NewClient("http://127.0.0.1:1", "")
	if _, err := Push(t.Context(), CollectOptions{Root: options.Root, Host: options.Host, ChatDir: options.ChatDir, All: true}, failing, nil); err == nil {
		t.Error("推送失败应报错")
	}

	// 采集就失败（根目录为空）时连一次请求都不发
	if _, err := Push(t.Context(), CollectOptions{}, client, nil); err == nil || !strings.Contains(err.Error(), "缺少记录根目录") {
		t.Errorf("采集失败应原样返回：%v", err)
	}
}

// Push 的状态文件边界：没配 StatePath 不落盘；有 StatePath 但一条都没推成功也不落盘；
// 状态写不下去（父路径是普通文件）时把错误报出来，而不是假装成功。
func TestPushStatePathBoundaries(t *testing.T) {
	serverStore, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	server := fakeServer(t, serverStore)
	client := NewClient(server.URL, "")

	// 没配 StatePath：推送照常，但没有任何状态文件产生
	options := pushOptions(t)
	options.StatePath = ""
	result, err := Push(t.Context(), options, client, nil)
	if err != nil || result.Stored != 1 {
		t.Fatalf("无状态路径推送 = %+v err=%v", result, err)
	}

	// StatePath 指向一个不可能写入的位置：报错（而不是静默丢状态）
	blocked := filepath.Join(t.TempDir(), "file")
	if err := writeFile(blocked, "x"); err != nil {
		t.Fatal(err)
	}
	options = pushOptions(t)
	options.StatePath = filepath.Join(blocked, "state.json")
	if _, err := Push(t.Context(), options, client, nil); err == nil {
		t.Error("状态写不下去时应报错")
	}

	// 状态路径是空目录字符串（TrimSpace 后为空）等同没配
	options = pushOptions(t)
	options.StatePath = "  "
	if result, err := Push(t.Context(), options, client, nil); err != nil || result.Stored != 1 {
		t.Fatalf("空白状态路径 = %+v err=%v", result, err)
	}
}

// 推送失败时不写状态：下一次仍会重试同一条（增量状态只记成功的）。
func TestPushFailureKeepsStateUntouched(t *testing.T) {
	options := pushOptions(t)
	// 先写一个「已推送」状态，再让推送失败，确认状态没被改写
	existing := State{"node-1/other/s-9": {Size: 1, ModTime: "2026-09-15T00:00:00Z"}}
	if err := existing.Save(options.StatePath); err != nil {
		t.Fatal(err)
	}
	client := NewClient("http://127.0.0.1:1", "")
	if _, err := Push(t.Context(), options, client, nil); err == nil {
		t.Fatal("推送失败应报错")
	}
	state := LoadState(options.StatePath)
	if len(state) != 1 || state["node-1/other/s-9"].Size != 1 {
		t.Errorf("失败推送不该改写状态：%+v", state)
	}
}
