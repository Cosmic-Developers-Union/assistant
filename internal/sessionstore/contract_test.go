package sessionstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 落盘契约：记录是原样 jsonl（逐行一字不改），元数据字段名与文档一致；读取
// 一条坏元数据不应让整个列表失败（跳过是刻意的，不是崩溃点）。
func TestStoreOnDiskContract(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := Open(root, OpenOptions{Host: "node-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if store.Root() != root {
		t.Errorf("Root() = %q, want %q", store.Root(), root)
	}
	key := Key{Host: "node-1", Project: "p-1", Session: "s-1"}
	if _, err := store.Put(Session{Meta: Meta{Key: key, Source: "chat"}, Lines: []string{"a", "b\n", "c\r\n"}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// jsonl 原样落盘，行尾被规范化成单个 \n
	data, err := os.ReadFile(filepath.Join(root, "node-1", "p-1", "s-1.jsonl"))
	if err != nil {
		t.Fatalf("读取会话文件: %v", err)
	}
	if string(data) != "a\nb\nc\n" {
		t.Errorf("jsonl 落盘内容 = %q", data)
	}

	// 元数据字段名（列表/检索/展示都按这些键解）
	rawMeta, err := os.ReadFile(filepath.Join(root, "node-1", "p-1", "s-1.meta.json"))
	if err != nil {
		t.Fatalf("读取元数据: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(rawMeta, &document); err != nil {
		t.Fatalf("元数据不是 JSON：%v", err)
	}
	for _, field := range []string{"host", "project", "session", "source", "lines", "bytes", "updated_at", "pushed_at"} {
		if _, ok := document[field]; !ok {
			t.Errorf("元数据缺字段 %q：%s", field, rawMeta)
		}
	}
	if document["lines"] != float64(3) || document["bytes"] != float64(6) {
		t.Errorf("lines/bytes = %v/%v", document["lines"], document["bytes"])
	}

	// 一条坏元数据（不可解析）与一条读不到的（指向目录）都被跳过，好记录仍可列出
	if err := os.WriteFile(filepath.Join(root, "node-1", "p-1", "broken.meta.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node-1", "p-1", "dir.meta.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	metas, err := store.List(Filter{}, 0)
	if err != nil || len(metas) != 1 || metas[0].Session != "s-1" {
		t.Fatalf("List = %+v err=%v", metas, err)
	}
	// 会话字段缺失时用文件名兜底
	if err := os.WriteFile(filepath.Join(root, "node-1", "p-1", "orphan.meta.json"), []byte(`{"source":"review"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	metas, err = store.List(Filter{}, 0)
	if err != nil || len(metas) != 2 {
		t.Fatalf("List = %+v err=%v", metas, err)
	}
	sessions := make([]string, 0, len(metas))
	for _, meta := range metas {
		sessions = append(sessions, meta.Session)
	}
	if !strings.Contains(strings.Join(sessions, ","), "orphan") {
		t.Errorf("缺 session 字段应回退文件名：%v", sessions)
	}
}

// Store.Open 的边界：根目录为空、manifest 坏档（不能当空库静默重建，那会丢掉身份）、
// Root 指向一个文件、以及目录建不出来。
func TestStoreOpenBoundaries(t *testing.T) {
	if _, err := Open("  ", OpenOptions{}); err == nil || !strings.Contains(err.Error(), "缺少记录库目录") {
		t.Errorf("空目录应报错：%v", err)
	}

	// manifest 坏档：报错而不是当空目录重建
	corrupt := t.TempDir()
	if err := os.WriteFile(filepath.Join(corrupt, ManifestFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(corrupt, OpenOptions{}); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("坏 manifest 应报错：%v", err)
	}

	// Root 是文件：既不能当空目录，也建不出子路径
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blocked, OpenOptions{}); err == nil {
		t.Error("根目录是普通文件时应报错")
	}
	if _, err := Open(filepath.Join(blocked, "inner"), OpenOptions{}); err == nil {
		t.Error("父路径是普通文件时应报错")
	}
}

// Key.Valid / String：三段都非空且不含路径分隔符（防目录穿越）。
func TestKeyValidAndString(t *testing.T) {
	if got := (Key{Host: "h", Project: "p", Session: "s"}).String(); got != "h/p/s" {
		t.Errorf("String() = %q", got)
	}
	cases := []struct {
		name string
		key  Key
		want bool
	}{
		{"完整", Key{Host: "h", Project: "p", Session: "s"}, true},
		{"缺会话", Key{Host: "h", Project: "p"}, false},
		{"只有空格", Key{Host: "h", Project: " ", Session: "s"}, false},
		{"带斜杠", Key{Host: "h", Project: "a/b", Session: "s"}, false},
		{"带反斜杠", Key{Host: `h\i`, Project: "p", Session: "s"}, false},
		{"全空", Key{}, false},
	}
	for _, testCase := range cases {
		if got := testCase.key.Valid(); got != testCase.want {
			t.Errorf("%s: Valid() = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// Filter.match 的各维过滤与时间下界（时间戳缺失时视为不匹配）。
func TestFilterMatch(t *testing.T) {
	meta := Meta{
		Key:          Key{Host: "h", Project: "p", Session: "s"},
		Conversation: "c-1", Source: "review", UpdatedAt: "2026-09-15T11:31:04Z",
	}
	cases := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"零值全过", Filter{}, true},
		{"命中键", Filter{Host: "h", Project: "p", Session: "s"}, true},
		{"主机不符", Filter{Host: "other"}, false},
		{"项目不符", Filter{Project: "other"}, false},
		{"会话不符", Filter{Session: "other"}, false},
		{"来源命中", Filter{Source: "review"}, true},
		{"来源不符", Filter{Source: "chat"}, false},
		{"会话实体命中", Filter{Conversation: "c-1"}, true},
		{"会话实体不符", Filter{Conversation: "c-2"}, false},
		{"时间下界之内", Filter{Since: mustParse(t, "2026-09-15T00:00:00Z")}, true},
		{"时间下界之后", Filter{Since: mustParse(t, "2026-09-16T00:00:00Z")}, false},
	}
	for _, testCase := range cases {
		if got := testCase.filter.match(meta); got != testCase.want {
			t.Errorf("%s: match() = %v, want %v", testCase.name, got, testCase.want)
		}
	}
	// UpdatedAt 缺失/坏档：带时间下界的过滤一律不匹配（不猜时间）
	if (Filter{Since: mustParse(t, "2020-01-01T00:00:00Z")}).match(Meta{}) {
		t.Error("时间戳缺失时不应匹配时间下界")
	}
	if (Filter{Since: mustParse(t, "2020-01-01T00:00:00Z")}).match(Meta{UpdatedAt: "不是时间"}) {
		t.Error("时间戳坏档时不应匹配时间下界")
	}
}

// 空库：列表/会话实体/检索都返回空而非报错；limit 与检索上限生效。
func TestStoreEmptyAndLimits(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if metas, err := store.List(Filter{}, 10); err != nil || len(metas) != 0 {
		t.Fatalf("空库 List = %+v err=%v", metas, err)
	}
	if counts, err := store.Conversations(); err != nil || len(counts) != 0 {
		t.Fatalf("空库 Conversations = %v err=%v", counts, err)
	}
	if _, err := store.Search("x", Filter{}, 10); err != nil {
		t.Errorf("空库检索不该报错：%v", err)
	}
	if _, err := store.Read(Key{Host: "nobody", Project: "p", Session: "s"}, 0, 0); err == nil {
		t.Error("读不存在的记录应报错")
	} else if _, ok := errors.AsType[*fs.PathError](err); !ok {
		t.Errorf("读不存在的记录应返回文件错误（便于调用方区分）：%v", err)
	}
	if _, err := store.Read(Key{Host: "h", Project: "p"}, 0, 0); err == nil {
		t.Error("非法键应被拒绝")
	}

	// 三条记录：列表按 limit 截断，检索按命中上限截断
	for index, name := range []string{"s-1", "s-2", "s-3"} {
		if _, err := store.Put(Session{
			Meta:  Meta{Key: Key{Host: "h", Project: "p", Session: name}, UpdatedAt: "2026-09-1" + string(rune('0'+index)) + "T00:00:00Z"},
			Lines: []string{`{"type":"user","message":{"role":"user","content":"同一句话"}}`},
		}); err != nil {
			t.Fatalf("Put(%s): %v", name, err)
		}
	}
	if metas, err := store.List(Filter{}, 2); err != nil || len(metas) != 2 {
		t.Fatalf("limit 未生效：%+v err=%v", metas, err)
	}
	matches, err := store.Search("同一句话", Filter{}, 1)
	if err != nil || len(matches) != 1 {
		t.Fatalf("检索 limit 未生效：%+v err=%v", matches, err)
	}
	if matches[0].Key.Session != "s-3" {
		t.Errorf("列表应按更新时间倒序，首个命中 = %+v", matches[0].Key)
	}
	// limit<=0 走默认上限（50），仍能拿到全部 3 条
	if matches, err := store.Search("同一句话", Filter{}, 0); err != nil || len(matches) != 3 {
		t.Fatalf("默认检索上限：%+v err=%v", matches, err)
	}
}

// Read 的 offset/limit 与坏行：坏行被跳过（记录是原样的，不因一行坏掉整条不可读）。
func TestStoreReadOffsetsAndBadLines(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	key := Key{Host: "h", Project: "p", Session: "s"}
	if _, err := store.Put(Session{Meta: Meta{Key: key}, Lines: []string{"不是 JSON", "", sampleTranscript}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	all, err := store.Read(key, 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("坏行不该让整条记录读不出来")
	}
	limited, err := store.Read(key, 0, 1)
	if err != nil || len(limited) != 1 || limited[0].Line != 3 {
		t.Fatalf("limit 未生效：%+v err=%v", limited, err)
	}
	// offset 按「可读消息」计数（行号仍在，便于定位原文）
	skipped, err := store.Read(key, 2, 0)
	if err != nil || len(skipped) != len(all)-2 {
		t.Fatalf("offset 未生效：%+v err=%v", skipped, err)
	}
}

func mustParse(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("解析时间 %q: %v", value, err)
	}
	return parsed
}
