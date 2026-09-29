package sessionindex

import (
	"os"
	"path/filepath"
	"testing"
)

// brokenStore 返回一个底层连接已被关闭的 Store：db 字段非 nil，但任何查询都会
// 报 "database is closed"。用它集中打各方法的错误返回路径——这些分支靠正常数据
// 走不到，真机上对应的是库文件损坏或被外力删除。
func brokenStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatalf("关闭底层连接失败：%v", err)
	}
	return store
}

func TestBrokenStoreErrors(t *testing.T) {
	store := brokenStore(t)
	key := Key{Host: "h", Project: "p", Session: "s"}

	if _, _, _, err := store.Watermark(key); err == nil {
		t.Error("Watermark 应报错")
	}
	if err := store.Upsert(sampleRecord("p", "s"), []Message{{Line: 1, Role: "user", Text: "x"}}); err == nil {
		t.Error("Upsert 应报错")
	}
	if _, err := store.List(Filter{}, 0); err == nil {
		t.Error("List 应报错")
	}
	if _, err := store.Read(key, 0, 0); err == nil {
		t.Error("Read 应报错")
	}
	if _, err := store.Search("abc", Filter{}, 0); err == nil {
		t.Error("Search 应报错")
	}
	if _, err := store.Conversations(); err == nil {
		t.Error("Conversations 应报错")
	}
}

// TestOpenMkdirFails 钉住父目录建不出来时的返回。
func TestOpenMkdirFails(t *testing.T) {
	// 把一个普通文件当父目录，MkdirAll 必然失败。
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备文件失败：%v", err)
	}
	if _, err := Open(filepath.Join(file, "index.sqlite3")); err == nil {
		t.Fatal("父路径不是目录时 Open 应报错")
	}
}

func TestOpenSchemaFails(t *testing.T) {
	// 把路径指向一个已存在的目录：sql.Open 是惰性的不会报错，建表时才会失败。
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Fatal("路径是目录时建表应失败")
	}
}

func TestCompareRecordTie(t *testing.T) {
	same := "2026-09-29T10:00:00Z"
	a := Record{Key: Key{Host: "h", Project: "p", Session: "a"}, UpdatedAt: same}
	b := Record{Key: Key{Host: "h", Project: "p", Session: "b"}, UpdatedAt: same}
	if got := compareRecord(a, b); got >= 0 {
		t.Fatalf("同刻应按键升序，a vs b = %d", got)
	}
	if got := compareRecord(b, a); got <= 0 {
		t.Fatalf("同刻应按键升序，b vs a = %d", got)
	}
}

// TestScanRecordError 直接喂一个列数不匹配的结果集，钉住扫描失败的返回。
func TestScanRecordError(t *testing.T) {
	store := openTestStore(t)
	row := store.db.QueryRow(`SELECT 1`)
	if _, err := scanRecord(row); err == nil {
		t.Fatal("列数不匹配时 scanRecord 应报错")
	}
}

// TestReadLimitOnlyAndOffsetOnly 覆盖 Read 的三种 LIMIT/OFFSET 组合。
func TestReadLimitOnlyAndOffsetOnly(t *testing.T) {
	store := openTestStore(t)
	record := sampleRecord("proj", "sess")
	if err := store.Upsert(record, []Message{
		{Line: 1, Role: "user", Text: "一"},
		{Line: 2, Role: "assistant", Text: "二"},
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	limitOnly, err := store.Read(record.Key, 0, 1)
	if err != nil || len(limitOnly) != 1 {
		t.Fatalf("仅 limit：得到 %d 条 err=%v", len(limitOnly), err)
	}
	offsetOnly, err := store.Read(record.Key, 1, 0)
	if err != nil || len(offsetOnly) != 1 || offsetOnly[0].Text != "二" {
		t.Fatalf("仅 offset：得到 %+v err=%v", offsetOnly, err)
	}
}

// 下面四个用例通过破坏索引自身的表来打 replaceMessages 的错误分支。
// 这不是为凑覆盖率造的假场景：索引是幂等重建的派生物，磁盘被外力动过
// （表被删、结构被改）时归档必须报错而不是静默写入半个会话。

// TestUpsertFailsWhenFTSTableMissing 覆盖「清理旧全文索引」失败（读得到旧消息，
// 但 FTS 表没了）。
func TestUpsertFailsWhenFTSTableMissing(t *testing.T) {
	store := openTestStore(t)
	record := sampleRecord("proj", "sess")
	if err := store.Upsert(record, []Message{{Line: 1, Role: "user", Text: "旧内容"}}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	if _, err := store.db.Exec(`DROP TABLE messages_fts`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if err := store.Upsert(record, []Message{{Line: 1, Role: "user", Text: "新内容"}}); err == nil {
		t.Fatal("FTS 表缺失时 Upsert 应报错")
	}
}

// TestUpsertFailsWhenMessageReadFails 覆盖「读取旧消息」失败。
func TestUpsertFailsWhenMessageReadFails(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(`DROP TABLE messages_fts`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if _, err := store.db.Exec(`DROP TABLE messages`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if err := store.Upsert(sampleRecord("proj", "sess"), []Message{{Line: 1, Role: "user", Text: "x"}}); err == nil {
		t.Fatal("messages 表缺失时 Upsert 应报错")
	}
}

// TestUpsertFailsWhenMessageInsertFails 覆盖「写入消息」失败：messages 表结构被
// 改得缺列，读旧消息仍成功，插入新消息才失败。
func TestUpsertFailsWhenMessageInsertFails(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(`DROP TABLE messages_fts`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if _, err := store.db.Exec(`DROP TABLE messages`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if _, err := store.db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT, host TEXT, project TEXT, session TEXT,
		line INTEGER, role TEXT, text TEXT)`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if err := store.Upsert(sampleRecord("proj", "sess"), []Message{{Line: 1, Role: "user", Text: "x"}}); err == nil {
		t.Fatal("messages 表缺列时 Upsert 应报错")
	}
}

// TestUpsertFailsWhenFTSInsertFails 覆盖「写入全文索引」失败：全新会话没有旧消息
// 可清，走到插入 FTS 那一步才失手。
func TestUpsertFailsWhenFTSInsertFails(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(`DROP TABLE messages_fts`); err != nil {
		t.Fatalf("破坏索引失败：%v", err)
	}
	if err := store.Upsert(sampleRecord("proj", "sess"), []Message{{Line: 1, Role: "user", Text: "x"}}); err == nil {
		t.Fatal("FTS 表缺失时写入新消息应报错")
	}
}
