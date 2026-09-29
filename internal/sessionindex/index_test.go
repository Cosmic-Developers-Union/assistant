package sessionindex

import (
	"path/filepath"
	"testing"
	"time"
)

// openTestStore 开一个临时库；t.Cleanup 关掉。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatalf("打开索引失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func sampleRecord(project, session string) Record {
	return Record{
		Key:        Key{Host: "host-a", Project: project, Session: session},
		Source:     "review",
		Title:      "评审 " + session,
		Model:      "claude-sonnet-5-5",
		Lines:      2,
		Bytes:      100,
		UpdatedAt:  "2026-09-29T10:00:00Z",
		ArchivedAt: "2026-09-29T10:00:01Z",
		Size:       4096,
		ModTime:    "2026-09-29T09:59:00Z",
	}
}

// TestSearchTrigramChinese 是本设计的关键前提：会话记录以中文为主，默认的
// unicode61 分词器会把整句当一个 token，中文子串检索必然失效。这里用中文
// 子串钉住 trigram 的行为——它必须能查到词中间的片段，而不是只匹配整串。
func TestSearchTrigramChinese(t *testing.T) {
	store := openTestStore(t)
	record := sampleRecord("proj", "sess-1")
	messages := []Message{
		{Line: 1, Role: "user", Text: "帮我看一下这次发布的会议记录", Timestamp: "2026-09-29T09:00:00Z"},
		{Line: 2, Role: "assistant", Text: "会议记录已归档，检索索引也建好了", Timestamp: "2026-09-29T09:00:01Z"},
	}
	if err := store.Upsert(record, messages); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	// "记录" 是两个汉字、六个字节，但只有 2 个 rune——按实现走 LIKE 分支。
	// 真正考验 trigram 的是下面这个 3 字的中文词。
	for _, query := range []string{"记录", "会议记录", "归档", "索引也建好了"} {
		hits, err := store.Search(query, Filter{}, 10)
		if err != nil {
			t.Fatalf("检索 %q 失败：%v", query, err)
		}
		if len(hits) == 0 {
			t.Fatalf("检索 %q 没有命中；trigram 对中文子串失效，须退回 LIKE 方案", query)
		}
	}
}

// TestSearchTrigramEnglishCaseInsensitive 钉住 case_sensitive=0（LIKE 语义）：
// 旧实现是 strings.Contains(strings.ToLower(..))，检索行为必须保持大小写不敏感。
func TestSearchTrigramEnglishCaseInsensitive(t *testing.T) {
	store := openTestStore(t)
	if err := store.Upsert(sampleRecord("proj", "sess-2"), []Message{
		{Line: 1, Role: "assistant", Text: "Deployed the Release CANDIDATE to staging"},
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	for _, query := range []string{"release", "RELEASE", "candidate", "Deployed"} {
		hits, err := store.Search(query, Filter{}, 10)
		if err != nil {
			t.Fatalf("检索 %q 失败：%v", query, err)
		}
		if len(hits) != 1 {
			t.Fatalf("检索 %q 命中 %d 条，期望 1 条", query, len(hits))
		}
	}
}

// TestSearchShortQueryFallback 钉住 <3 rune 的检索词走 LIKE 全表扫：
// trigram 至少要三个字符才建得出索引，直接 MATCH 会静默返回空。
func TestSearchShortQueryFallback(t *testing.T) {
	store := openTestStore(t)
	if err := store.Upsert(sampleRecord("proj", "sess-3"), []Message{
		{Line: 1, Role: "user", Text: "ab 记 x"},
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	for _, query := range []string{"记", "ab", "x"} {
		hits, err := store.Search(query, Filter{}, 10)
		if err != nil {
			t.Fatalf("检索 %q 失败：%v", query, err)
		}
		if len(hits) == 0 {
			t.Fatalf("短词 %q 没有命中；LIKE 回退失效", query)
		}
	}
}

// TestSearchLiteralSpecials 钉住检索词里的 FTS/LIKE 语法字符按字面处理：
// ftsPhrase 负责引号转义，escapeLike 负责 % _ \。
func TestSearchLiteralSpecials(t *testing.T) {
	store := openTestStore(t)
	if err := store.Upsert(sampleRecord("proj", "sess-4"), []Message{
		{Line: 1, Role: "user", Text: `统计 100% 完成，路径 C:\tmp 与 a_b`},
	}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	for _, query := range []string{"100%", `C:\tmp`, "a_b"} {
		hits, err := store.Search(query, Filter{}, 10)
		if err != nil {
			t.Fatalf("检索 %q 失败：%v", query, err)
		}
		if len(hits) == 0 {
			t.Fatalf("按字面检索 %q 没有命中", query)
		}
	}
	// LIKE 分支里的 % 必须当字面量：随便一个不存在的 3 字词不该被 % 通配命中。
	noHits, err := store.Search("zzz", Filter{}, 10)
	if err != nil {
		t.Fatalf("检索 zzz 失败：%v", err)
	}
	if len(noHits) != 0 {
		t.Fatalf("检索 zzz 命中 %d 条，期望 0 条", len(noHits))
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Search("   ", Filter{}, 10); err == nil {
		t.Fatal("空检索词应当报错")
	}
}

// TestSearchFilterPushdown 钉住过滤条件确实生效（且投影了正确的会话字段）。
func TestSearchFilterPushdown(t *testing.T) {
	store := openTestStore(t)
	recordA := sampleRecord("proj-a", "sess-a")
	if err := store.Upsert(recordA, []Message{{Line: 1, Role: "user", Text: "shared keyword"}}); err != nil {
		t.Fatalf("写入 A 失败：%v", err)
	}
	recordB := sampleRecord("proj-b", "sess-b")
	if err := store.Upsert(recordB, []Message{{Line: 1, Role: "user", Text: "shared keyword"}}); err != nil {
		t.Fatalf("写入 B 失败：%v", err)
	}

	hits, err := store.Search("shared", Filter{Project: "proj-b"}, 10)
	if err != nil {
		t.Fatalf("检索失败：%v", err)
	}
	if len(hits) != 1 || hits[0].Project != "proj-b" {
		t.Fatalf("过滤未生效，命中 %+v", hits)
	}
}

func TestUpsertReplacesMessages(t *testing.T) {
	store := openTestStore(t)
	record := sampleRecord("proj", "sess")
	if err := store.Upsert(record, []Message{
		{Line: 1, Role: "user", Text: "第一版内容"},
		{Line: 2, Role: "assistant", Text: "第一版回复"},
	}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	// 整体替换：旧的第二行必须消失，且全文索引里也不能留残影。
	if err := store.Upsert(record, []Message{{Line: 1, Role: "user", Text: "第二版内容"}}); err != nil {
		t.Fatalf("替换写入失败：%v", err)
	}
	messages, err := store.Read(record.Key, 0, 0)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if len(messages) != 1 || messages[0].Text != "第二版内容" {
		t.Fatalf("替换后消息为 %+v", messages)
	}
	stale, err := store.Search("第一版回复", Filter{}, 10)
	if err != nil {
		t.Fatalf("检索失败：%v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("替换后仍能检索到旧文本：%+v", stale)
	}
}

func TestWatermark(t *testing.T) {
	store := openTestStore(t)
	key := Key{Host: "h", Project: "p", Session: "s"}
	if _, _, ok, err := store.Watermark(key); err != nil || ok {
		t.Fatalf("未归档会话水位应为 ok=false，得到 ok=%v err=%v", ok, err)
	}
	record := sampleRecord("p", "s")
	record.Host = "h"
	if err := store.Upsert(record, nil); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	size, modTime, ok, err := store.Watermark(key)
	if err != nil || !ok {
		t.Fatalf("已归档会话水位应 ok=true，得到 ok=%v err=%v", ok, err)
	}
	if size != record.Size || modTime != record.ModTime {
		t.Fatalf("水位 = (%d, %q)，期望 (%d, %q)", size, modTime, record.Size, record.ModTime)
	}
}

func TestListOrderAndFilter(t *testing.T) {
	store := openTestStore(t)
	older := sampleRecord("proj", "old")
	older.UpdatedAt = "2026-09-29T08:00:00Z"
	newer := sampleRecord("proj", "new")
	newer.UpdatedAt = "2026-09-29T12:00:00Z"
	other := sampleRecord("other-proj", "x")
	other.UpdatedAt = "2026-09-29T13:00:00Z"
	for _, record := range []Record{older, newer, other} {
		if err := store.Upsert(record, nil); err != nil {
			t.Fatalf("写入失败：%v", err)
		}
	}

	all, err := store.List(Filter{}, 0)
	if err != nil {
		t.Fatalf("列出失败：%v", err)
	}
	want := []string{"other-proj/x", "proj/new", "proj/old"}
	if len(all) != len(want) {
		t.Fatalf("列出 %d 条，期望 %d 条", len(all), len(want))
	}
	for index, record := range all {
		if got := record.Project + "/" + record.Session; got != want[index] {
			t.Fatalf("第 %d 条为 %s，期望 %s", index, got, want[index])
		}
	}

	filtered, err := store.List(Filter{Project: "proj"}, 0)
	if err != nil {
		t.Fatalf("过滤列出失败：%v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("按 project 过滤得到 %d 条，期望 2 条", len(filtered))
	}

	limited, err := store.List(Filter{}, 1)
	if err != nil {
		t.Fatalf("限量列出失败：%v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 得到 %d 条", len(limited))
	}

	since, err := store.List(Filter{Since: time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)}, 0)
	if err != nil {
		t.Fatalf("按时间列出失败：%v", err)
	}
	if len(since) != 2 {
		t.Fatalf("按 Since 过滤得到 %d 条，期望 2 条", len(since))
	}
}

func TestReadPaging(t *testing.T) {
	store := openTestStore(t)
	record := sampleRecord("proj", "sess")
	messages := []Message{
		{Line: 1, Role: "user", Text: "一"},
		{Line: 2, Role: "assistant", Text: "二"},
		{Line: 3, Role: "user", Text: "三"},
	}
	if err := store.Upsert(record, messages); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	all, err := store.Read(record.Key, 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("全量读取得到 %d 条 err=%v", len(all), err)
	}
	paged, err := store.Read(record.Key, 1, 1)
	if err != nil || len(paged) != 1 || paged[0].Text != "二" {
		t.Fatalf("分页读取得到 %+v err=%v", paged, err)
	}
	if _, err := store.Read(Key{Host: "h", Project: "..", Session: "s"}, 0, 0); err == nil {
		t.Fatal("非法键读取应报错")
	}
}

func TestConversations(t *testing.T) {
	store := openTestStore(t)
	first := sampleRecord("proj", "sess-1")
	first.Conversation = "conv-1"
	second := sampleRecord("proj", "sess-2")
	second.Conversation = "conv-1"
	third := sampleRecord("proj", "sess-3") // 非聊天会话，conversation 为空
	for _, record := range []Record{first, second, third} {
		if err := store.Upsert(record, nil); err != nil {
			t.Fatalf("写入失败：%v", err)
		}
	}
	counts, err := store.Conversations()
	if err != nil {
		t.Fatalf("列出会话实体失败：%v", err)
	}
	if len(counts) != 1 || counts["conv-1"] != 2 {
		t.Fatalf("会话实体计数为 %+v，期望 {conv-1:2}", counts)
	}
}

func TestNilStoreSafe(t *testing.T) {
	var store *Store
	if err := store.Close(); err != nil {
		t.Fatalf("nil Close：%v", err)
	}
	if err := store.Upsert(sampleRecord("p", "s"), nil); err != nil {
		t.Fatalf("nil Upsert：%v", err)
	}
	if _, err := store.List(Filter{}, 0); err != nil {
		t.Fatalf("nil List：%v", err)
	}
	if _, _, _, err := store.Watermark(Key{Host: "h", Project: "p", Session: "s"}); err != nil {
		t.Fatalf("nil Watermark：%v", err)
	}
	if _, err := store.Read(Key{Host: "h", Project: "p", Session: "s"}, 0, 0); err != nil {
		t.Fatalf("nil Read：%v", err)
	}
	if _, err := store.Search("abc", Filter{}, 0); err != nil {
		t.Fatalf("nil Search：%v", err)
	}
	if _, err := store.Conversations(); err != nil {
		t.Fatalf("nil Conversations：%v", err)
	}
}

// TestOpenEmptyPath 沿用 statestore 的约定：路径为空表示未启用，返回 nil 而非报错。
func TestOpenEmptyPath(t *testing.T) {
	store, err := Open("  ")
	if err != nil || store != nil {
		t.Fatalf("Open(\"\") = (%v, %v)，期望 (nil, nil)", store, err)
	}
}

// TestOpenCreatesDirectory 钉住父目录会被创建（索引落在 state 目录下，首次运行
// 该目录可能还不存在）。
func TestOpenCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "index.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
}

func TestKeyValid(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		want bool
	}{
		{"正常", Key{Host: "h", Project: "p", Session: "s"}, true},
		{"空主机", Key{Host: "", Project: "p", Session: "s"}, false},
		{"空项目", Key{Host: "h", Project: "", Session: "s"}, false},
		{"空会话", Key{Host: "h", Project: "p", Session: ""}, false},
		{"点", Key{Host: "h", Project: ".", Session: "s"}, false},
		{"点点", Key{Host: "h", Project: "..", Session: "s"}, false},
		{"斜杠", Key{Host: "h", Project: "a/b", Session: "s"}, false},
		{"反斜杠", Key{Host: "h", Project: `a\b`, Session: "s"}, false},
		{"首尾空白", Key{Host: " h ", Project: "p", Session: "s"}, true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := item.key.Valid(); got != item.want {
				t.Fatalf("Valid() = %v，期望 %v", got, item.want)
			}
		})
	}
}

func TestKeyString(t *testing.T) {
	key := Key{Host: "h", Project: "p", Session: "s"}
	if got := key.String(); got != "h/p/s" {
		t.Fatalf("String() = %q", got)
	}
}

// TestUpsertRejectsInvalidKey 钉住校验发生在写库之前（否则穿越会落进对象 key）。
func TestUpsertRejectsInvalidKey(t *testing.T) {
	store := openTestStore(t)
	record := sampleRecord("..", "sess")
	if err := store.Upsert(record, nil); err == nil {
		t.Fatal("非法键应当被拒绝")
	}
}

func TestWatermarkInvalidKey(t *testing.T) {
	store := openTestStore(t)
	if _, _, _, err := store.Watermark(Key{Host: "h", Project: ".", Session: "s"}); err == nil {
		t.Fatal("非法键应当报错")
	}
}

// TestWatermarkNoRows 直接验证 sql.ErrNoRows 分支被正确吞掉（返回 ok=false）。
func TestWatermarkNoRows(t *testing.T) {
	store := openTestStore(t)
	_, _, ok, err := store.Watermark(Key{Host: "nope", Project: "nope", Session: "nope"})
	if err != nil {
		t.Fatalf("未归档键不应报错：%v", err)
	}
	if ok {
		t.Fatal("未归档键 ok 应为 false")
	}
}
