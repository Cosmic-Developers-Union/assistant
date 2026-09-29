package sessionstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
)

// fakeBlob 是内存版对象存储，替真 MinIO 走归档编排的逻辑测试。
type fakeBlob struct {
	objects map[string][]byte
	puts    int
	failPut bool
}

func (f *fakeBlob) Put(_ context.Context, key sessionindex.Key, data []byte) error {
	if f.failPut {
		return errors.New("上传失败")
	}
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[key.String()] = append([]byte(nil), data...)
	f.puts++
	return nil
}

func (f *fakeBlob) Get(_ context.Context, key sessionindex.Key) ([]byte, bool, error) {
	data, ok := f.objects[key.String()]
	return data, ok, nil
}

// newTestArchiver 造一个用内存 blob 与临时索引的归档器。
func newTestArchiver(t *testing.T, options CollectOptions) (*Archiver, *fakeBlob, *sessionindex.Store) {
	t.Helper()
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatalf("打开索引失败：%v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	blob := &fakeBlob{}
	archive := NewArchiver(options, blob, index, nil)
	if archive == nil {
		t.Fatal("NewArchiver 不该返回 nil")
	}
	return archive, blob, index
}

func TestNewArchiverDisabledWithoutStorage(t *testing.T) {
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if archive := NewArchiver(CollectOptions{}, nil, index, nil); archive != nil {
		t.Error("blob 为 nil 时应返回 nil（归档关闭）")
	}
	if archive := NewArchiver(CollectOptions{}, &fakeBlob{}, nil, nil); archive != nil {
		t.Error("index 为 nil 时应返回 nil（归档关闭）")
	}
}

// TestArchiveFlow 覆盖全量归档的主路径：首次全存、二次全跳过、内容变化后重存，
// 并核对对象存储的键与索引里的记录一致。
func TestArchiveFlow(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	archive, blob, index := newTestArchiver(t, CollectOptions{Root: root, Host: "node-1"})

	summary, err := archive.Archive(t.Context())
	if err != nil {
		t.Fatalf("首次归档：%v", err)
	}
	if summary.Scanned != 1 || summary.Stored != 1 || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("首次统计 = %+v", summary)
	}
	if _, ok := blob.objects["node-1/-tmp-work/s-1"]; !ok {
		t.Fatalf("对象未上传：%v", blob.objects)
	}

	// 第二次：水位未变，跳过、不再上传
	summary, err = archive.Archive(t.Context())
	if err != nil || summary.Skipped != 1 || summary.Stored != 0 {
		t.Fatalf("二次统计 = %+v err=%v", summary, err)
	}
	if blob.puts != 1 {
		t.Errorf("水位未变不该重传：puts=%d", blob.puts)
	}

	// 索引里可查
	records, err := index.List(Filter{}, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("索引记录 = %+v err=%v", records, err)
	}
	if records[0].Size == 0 || records[0].ModTime == "" {
		t.Errorf("水位未写进索引：%+v", records[0])
	}
	hits, err := index.Search("待评审", Filter{}, 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("归档后应可检索：%+v err=%v", hits, err)
	}

	// 内容变长：水位变化 → 重传、索引整体替换
	if err := writeFile(filepath.Join(root, "projects", "-tmp-work", "s-1.jsonl"), sampleTranscript+"\n"); err != nil {
		t.Fatal(err)
	}
	summary, err = archive.Archive(t.Context())
	if err != nil || summary.Stored != 1 {
		t.Fatalf("变化后统计 = %+v err=%v", summary, err)
	}
	if blob.puts != 2 {
		t.Errorf("变化后应重传：puts=%d", blob.puts)
	}
}

// TestArchiveChatMapping 钉住 chat 目录的映射：来源变 chat、会话实体/通道/标题带上。
func TestArchiveChatMapping(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	chatDir := filepath.Join(t.TempDir(), "chat")
	entryDir := filepath.Join(chatDir, "conv-1")
	if err := osMkdirAll(entryDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(entryDir, ChatSessionFile),
		`{"conversation_id":"c-1","session_id":"s-1","title":"发布讨论","model":"m","transport":"weixin"}`); err != nil {
		t.Fatal(err)
	}
	archive, _, index := newTestArchiver(t, CollectOptions{Root: root, Host: "node-1", ChatDir: chatDir})
	if _, err := archive.Archive(t.Context()); err != nil {
		t.Fatalf("归档：%v", err)
	}
	records, err := index.List(Filter{}, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("记录 = %+v err=%v", records, err)
	}
	record := records[0]
	if record.Source != "chat" || record.Conversation != "c-1" || record.Transport != "weixin" ||
		record.Title != "发布讨论" || record.Model != "m" {
		t.Errorf("chat 映射未带上：%+v", record)
	}
	// 计数走 conversation
	counts, err := index.Conversations()
	if err != nil || counts["c-1"] != 1 {
		t.Errorf("会话实体计数 = %+v err=%v", counts, err)
	}
}

// TestArchiveWithoutChatDir 非聊天会话：来源由项目名推断为 unknown，无会话实体。
func TestArchiveWithoutChatDir(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	archive, _, index := newTestArchiver(t, CollectOptions{Root: root, Host: "n"})
	if _, err := archive.Archive(t.Context()); err != nil {
		t.Fatalf("归档：%v", err)
	}
	records, _ := index.List(Filter{}, 0)
	if len(records) != 1 || records[0].Source != "unknown" {
		t.Fatalf("来源应为 unknown：%+v", records)
	}
	if counts, _ := index.Conversations(); len(counts) != 0 {
		t.Errorf("无聊天会话时不该有会话实体：%+v", counts)
	}
}

// TestArchiveSessionByID 覆盖每轮钩子的单条归档路径。
func TestArchiveSessionByID(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	archive, blob, _ := newTestArchiver(t, CollectOptions{Root: root, Host: "n"})

	if ok, err := archive.ArchiveSession(t.Context(), "不存在"); ok || err != nil {
		t.Errorf("不存在的会话 = ok=%v err=%v", ok, err)
	}
	ok, err := archive.ArchiveSession(t.Context(), "s-1")
	if err != nil || !ok {
		t.Fatalf("ArchiveSession = ok=%v err=%v", ok, err)
	}
	if _, found := blob.objects["n/-tmp-work/s-1"]; !found {
		t.Errorf("对象未上传：%v", blob.objects)
	}
	// 再归档一次：水位未变，ok=false（跳过）
	if ok, err := archive.ArchiveSession(t.Context(), "s-1"); err != nil || ok {
		t.Errorf("水位未变应跳过：ok=%v err=%v", ok, err)
	}
}

// TestArchivePutFailure 上传失败：计入 Failed、返回错误、索引不落记录。
func TestArchivePutFailure(t *testing.T) {
	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	archive, blob, index := newTestArchiver(t, CollectOptions{Root: root, Host: "n"})
	blob.failPut = true

	summary, err := archive.Archive(t.Context())
	if err == nil {
		t.Fatal("上传失败应返回错误")
	}
	if summary.Failed != 1 || summary.Stored != 0 {
		t.Errorf("统计 = %+v", summary)
	}
	if records, _ := index.List(Filter{}, 0); len(records) != 0 {
		t.Errorf("上传失败不该落索引：%+v", records)
	}
}

// TestArchiveEmptyRoot 空记录目录：一切为零，不报错。
func TestArchiveEmptyRoot(t *testing.T) {
	archive, _, _ := newTestArchiver(t, CollectOptions{Root: filepath.Join(t.TempDir(), "claude"), Host: "n"})
	summary, err := archive.Archive(t.Context())
	if err != nil || summary.Scanned != 0 {
		t.Fatalf("空目录 = %+v err=%v", summary, err)
	}
}

// TestArchiverNilSafe ptr 接收者为 nil 时全部方法是空操作。
func TestArchiverNilSafe(t *testing.T) {
	var archive *Archiver
	if err := archive.Close(); err != nil {
		t.Errorf("nil Close：%v", err)
	}
	if summary, err := archive.Archive(t.Context()); err != nil || summary.Scanned != 0 {
		t.Errorf("nil Archive = %+v err=%v", summary, err)
	}
	if ok, err := archive.ArchiveSession(t.Context(), "s"); ok || err != nil {
		t.Errorf("nil ArchiveSession = ok=%v err=%v", ok, err)
	}
}

// joinLines / extractAll 的退化输入。
func TestJoinLinesAndExtractAll(t *testing.T) {
	if got := joinLines(nil); got != nil {
		t.Errorf("空行集应返回 nil：%q", got)
	}
	if got := joinLines([]string{"a", "b"}); string(got) != "a\nb\n" {
		t.Errorf("joinLines = %q", got)
	}
	messages := extractAll(sampleLines())
	if len(messages) < 4 {
		t.Errorf("抽取消息数 = %d", len(messages))
	}
	roles := map[string]bool{}
	for _, message := range messages {
		roles[message.Role] = true
	}
	for _, role := range []string{"user", "assistant", "tool", "tool_result", "error"} {
		if !roles[role] {
			t.Errorf("缺角色 %s：%+v", role, roles)
		}
	}
}

// TestArchiveThroughS3Client 把归档器接到**真实 minio-go 客户端 + 进程内假 S3 端点 +
// 真实 SQLite 索引**上，走完整链路：归档 → 对象落桶（真 HTTP PUT）→ 索引可查（含中文
// 子串检索）→ 分页读回 → 对象内容与本地记录一致。
//
// 与 test/e2e 的区别：那边打真 MinIO 服务（验证 SigV4/服务端行为，本机无出网时自动
// 跳过）；这里是本进程内自洽的端到端，任何环境都能跑，覆盖到除「真服务端」以外的全部。
func TestArchiveThroughS3Client(t *testing.T) {
	server, bucket := newFakeS3(t)
	blob, err := NewS3Store(StorageConfig{Endpoint: server.URL, Bucket: "assistant-sessions"})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	if err := blob.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatalf("打开索引: %v", err)
	}
	defer index.Close()

	root := sampleClaudeRoot(t, "-tmp-work", "s-1", sampleTranscript)
	archive := NewArchiver(CollectOptions{Root: root, Host: "node-1"}, blob, index, nil)
	summary, err := archive.Archive(t.Context())
	if err != nil || summary.Stored != 1 {
		t.Fatalf("归档 = %+v err=%v", summary, err)
	}

	// 对象真的落在（假）桶里
	if _, ok := bucket.objects["node-1/-tmp-work/s-1.jsonl"]; !ok {
		t.Fatalf("对象未落桶：%v", bucket.objects)
	}
	// 索引里查得到，且中文子串能命中
	records, err := index.List(Filter{}, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("索引列表 = %+v err=%v", records, err)
	}
	hits, err := index.Search("待评审", Filter{}, 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("中文子串检索 = %+v err=%v", hits, err)
	}
	// 分页读
	if page, err := index.Read(records[0].Key, 1, 1); err != nil || len(page) != 1 {
		t.Fatalf("分页读 = %+v err=%v", page, err)
	}
	// 经真 minio 客户端读回，内容与本地记录一致
	payload, ok, err := blob.Get(t.Context(), records[0].Key)
	if err != nil || !ok || string(payload) != string(joinLines(sampleLines())) {
		t.Fatalf("读回对象 = ok=%v err=%v", ok, err)
	}
}
