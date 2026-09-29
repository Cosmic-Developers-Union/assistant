package sessionstore

import (
	"path/filepath"
	"strings"
	"testing"
)

// Scan/Find 面对坏目录树：根目录是普通文件时都报错，而不是当空目录。
func TestScanAndFindDirtyTree(t *testing.T) {
	file := filepath.Join(t.TempDir(), "claude-file")
	if err := writeFile(file, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(CollectOptions{Root: file}); err == nil {
		t.Error("Scan：根目录是普通文件应报错")
	}
	if _, _, err := Find(CollectOptions{Root: file}, "s-1"); err == nil {
		t.Error("Find：根目录是普通文件应报错")
	}
}

// readChatSessions 的脏输入：非目录条目、缺 session.json、坏 JSON、缺 session_id
// 一律跳过。
func TestReadChatSessionsDirtyEntries(t *testing.T) {
	chatDir := t.TempDir()
	// 非目录条目
	if err := writeFile(filepath.Join(chatDir, "stray.json"), "[]"); err != nil {
		t.Fatal(err)
	}
	// 目录但没有 session.json
	if err := osMkdirAll(filepath.Join(chatDir, "empty")); err != nil {
		t.Fatal(err)
	}
	// 坏 JSON
	broken := filepath.Join(chatDir, "broken")
	if err := osMkdirAll(broken); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(broken, ChatSessionFile), `{`); err != nil {
		t.Fatal(err)
	}
	// 缺 session_id
	noid := filepath.Join(chatDir, "noid")
	if err := osMkdirAll(noid); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(noid, ChatSessionFile), `{"conversation_id":"c"}`); err != nil {
		t.Fatal(err)
	}
	if got := readChatSessions(chatDir); len(got) != 0 {
		t.Errorf("脏条目应全部跳过：%+v", got)
	}
}

// readLines 读不到文件时返回错误（不吞）。
func TestReadLinesMissingFile(t *testing.T) {
	if _, err := readLines(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("读不到文件应报错")
	}
}

// archiveCandidate 面对读不到的文件时报错（而不是当作空记录归档）。
func TestArchiveCandidateReadFailure(t *testing.T) {
	archive, blob, _ := newTestArchiver(t, CollectOptions{Root: filepath.Join(t.TempDir(), "claude"), Host: "n"})
	candidate := Candidate{
		Key:     Key{Host: "n", Project: "p", Session: "s"},
		Path:    filepath.Join(t.TempDir(), "missing.jsonl"),
		Size:    1,
		ModTime: "2026-09-29T10:00:00Z",
	}
	if _, err := archive.archiveCandidate(t.Context(), candidate); err == nil {
		t.Error("读不到记录文件应报错")
	}
	if len(blob.keys()) != 0 {
		t.Errorf("失败不该上传：%v", blob.objects)
	}
}

// build 的修改时间解析回退：坏格式用当前时间兜底，不 panic、不产生空时间。
func TestBuildModTimeFallback(t *testing.T) {
	archive, _, _ := newTestArchiver(t, CollectOptions{Host: "n"})
	record, _ := archive.build(Candidate{
		Key:     Key{Host: "n", Project: "-tmp-work", Session: "s"},
		ModTime: "不是时间",
	}, []string{`{"type":"user","message":{"role":"user","content":"你好"}}`}, 10)
	if record.CreatedAt == "" || record.UpdatedAt == "" {
		t.Errorf("坏修改时间应有兜底时间：%+v", record)
	}
	if record.Lines != 1 || record.Bytes != 10 {
		t.Errorf("行数/字节 = %d/%d", record.Lines, record.Bytes)
	}
}

// Archive 的扫描失败：根目录不可读时直接返回错误。
func TestArchiveScanFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "claude-file")
	if err := writeFile(file, "x"); err != nil {
		t.Fatal(err)
	}
	archive, _, _ := newTestArchiver(t, CollectOptions{Root: file, Host: "n"})
	if _, err := archive.Archive(t.Context()); err == nil {
		t.Error("根目录不可读应报错")
	}
}

// 索引被关掉后归档：水位读取失败 → 计入 Failed、返回错误。
func TestArchiveWatermarkFailure(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	archive, _, index := newTestArchiver(t, CollectOptions{Root: root, Host: "n"})
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := archive.Archive(t.Context())
	if err == nil || summary.Failed != 1 {
		t.Fatalf("索引不可用应计入失败：%+v err=%v", summary, err)
	}
}

// 索引被关掉后跑 MCP 工具：每个工具都回 isError 的 result，而不是 panic。
func TestMCPToolsWithClosedIndex(t *testing.T) {
	index := seedIndex(t)
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_search","arguments":{"query":"队列"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"session_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"session_read","arguments":{"host":"h","project":"p","session":"s"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"conversation_list","arguments":{}}}`,
	}
	var out strings.Builder
	if err := RunMCP(t.Context(), strings.NewReader(strings.Join(requests, "\n")+"\n"), &out, MCPOptions{Index: index}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	text := out.String()
	if count := strings.Count(text, `"isError":true`); count != 4 {
		t.Errorf("四个工具都该回 isError，实际 %d 次：\n%s", count, text)
	}
}

// blob.keys 是本测试用的便捷视图（对象键列表）。
func (f *fakeBlob) keys() []string {
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	return keys
}
