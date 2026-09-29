package sessionstore

import (
	"path/filepath"
	"testing"
)

// Scan 的边界：非目录条目跳过、非 .jsonl 跳过、子目录跳过、projects 缺失返回空、
// 根目录是普通文件时报错。
func TestScanBoundaries(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude")
	projectDir := filepath.Join(root, "projects", "-tmp-work")
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "s-1.jsonl"), sampleTranscript); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "notes.txt"), "忽略我"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(root, "projects", "stray.txt"), "忽略我"); err != nil {
		t.Fatal(err)
	}
	if err := osMkdirAll(filepath.Join(projectDir, "nested.jsonl")); err != nil {
		t.Fatal(err)
	}

	candidates, err := Scan(CollectOptions{Root: root, Host: "node-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("应只扫到 1 条：%+v", candidates)
	}
	candidate := candidates[0]
	if candidate.Key.String() != "node-1/-tmp-work/s-1" {
		t.Errorf("键 = %s", candidate.Key.String())
	}
	if candidate.Path == "" || candidate.Size == 0 || candidate.ModTime == "" {
		t.Errorf("候选缺少 stat 信息：%+v", candidate)
	}

	// 缺 projects 目录：空结果不算错（干净机器）
	if got, err := Scan(CollectOptions{Root: filepath.Join(dir, "empty")}); err != nil || len(got) != 0 {
		t.Errorf("缺 projects 应为空：%+v err=%v", got, err)
	}
	// 缺 Root：报错
	if _, err := Scan(CollectOptions{}); err == nil {
		t.Error("缺记录根目录应报错")
	}
	// 根目录是普通文件：读 projects 失败应报错
	blockedRoot := filepath.Join(dir, "root-file")
	if err := writeFile(blockedRoot, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(CollectOptions{Root: blockedRoot}); err == nil {
		t.Error("根目录是普通文件时应报错")
	}
}

// Scan 未给 Host 时回落到主机名（非空）。
func TestScanDefaultsHost(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	candidates, err := Scan(CollectOptions{Root: root})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Scan: %+v err=%v", candidates, err)
	}
	if candidates[0].Key.Host == "" {
		t.Error("缺省主机标签不应为空")
	}
}

// Find：按会话 id 跨项目定位、空 id 返回 false、缺失目录返回 false。
func TestFind(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude")
	projectDir := filepath.Join(root, "projects", "-tmp-work")
	if err := osMkdirAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(projectDir, "s-1.jsonl"), sampleTranscript); err != nil {
		t.Fatal(err)
	}
	options := CollectOptions{Root: root, Host: "n"}

	if _, ok, err := Find(options, "   "); ok || err != nil {
		t.Errorf("空会话 id = ok=%v err=%v", ok, err)
	}
	if _, ok, err := Find(CollectOptions{Root: filepath.Join(dir, "empty")}, "s-1"); ok || err != nil {
		t.Errorf("缺 projects 目录 = ok=%v err=%v", ok, err)
	}
	found, ok, err := Find(options, "s-1")
	if err != nil || !ok || found.Key.String() != "n/-tmp-work/s-1" {
		t.Fatalf("Find = %+v ok=%v err=%v", found, ok, err)
	}
	if _, ok, _ := Find(options, "不存在"); ok {
		t.Error("不存在的会话应 ok=false")
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
