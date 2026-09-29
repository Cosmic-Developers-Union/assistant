package sessionstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
)

// fakeObjectStore 是实现 objectStore 的内存假实现，替真 MinIO 走键校验/布局/错误包装。
type fakeObjectStore struct {
	objects  map[string][]byte
	putErr   error
	getErr   error
	bucketOK error
}

func (f *fakeObjectStore) put(_ context.Context, object string, data []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[object] = append([]byte(nil), data...)
	return nil
}

func (f *fakeObjectStore) get(_ context.Context, object string) ([]byte, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	data, ok := f.objects[object]
	return data, ok, nil
}

func (f *fakeObjectStore) ensureBucket(context.Context) error { return f.bucketOK }

// Put/Get 的成功与错误路径：走假对象存储，覆盖键校验、键布局与错误包装。
func TestS3StorePutGet(t *testing.T) {
	inner := &fakeObjectStore{}
	store := &S3Store{store: inner, prefix: "sessions/"}
	key := sessionindex.Key{Host: "node-1", Project: "-tmp-work", Session: "s-1"}

	if err := store.Put(t.Context(), key, []byte("内容")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := inner.objects["sessions/node-1/-tmp-work/s-1.jsonl"]; !ok {
		t.Fatalf("对象键不对：%v", inner.objects)
	}
	data, ok, err := store.Get(t.Context(), key)
	if err != nil || !ok || string(data) != "内容" {
		t.Fatalf("Get = %q ok=%v err=%v", data, ok, err)
	}
	// 不存在的对象：ok=false 且不是错误
	if _, ok, err := store.Get(t.Context(), sessionindex.Key{Host: "h", Project: "p", Session: "缺失"}); ok || err != nil {
		t.Errorf("缺失对象 = ok=%v err=%v", ok, err)
	}

	// 底层失败：错误被包装上语境
	inner.putErr = errors.New("网络断了")
	if err := store.Put(t.Context(), key, []byte("x")); err == nil || !strings.Contains(err.Error(), "上传会话记录") {
		t.Errorf("Put 失败文案 = %v", err)
	}
	inner.putErr = nil
	inner.getErr = errors.New("网络断了")
	if _, _, err := store.Get(t.Context(), key); err == nil || !strings.Contains(err.Error(), "读取会话记录") {
		t.Errorf("Get 失败文案 = %v", err)
	}
}

// EnsureBucket 透传底层结果；未启用时报错。
func TestS3StoreEnsureBucket(t *testing.T) {
	store := &S3Store{store: &fakeObjectStore{}}
	if err := store.EnsureBucket(t.Context()); err != nil {
		t.Errorf("EnsureBucket: %v", err)
	}
	var nilStore *S3Store
	if err := nilStore.EnsureBucket(t.Context()); err == nil {
		t.Error("未启用时应报错")
	}
	if err := (&S3Store{}).EnsureBucket(t.Context()); err == nil {
		t.Error("store 为空时应报错")
	}
}

func TestStorageConfigEnabled(t *testing.T) {
	cases := []struct {
		name   string
		config StorageConfig
		want   bool
	}{
		{"完整", StorageConfig{Endpoint: "minio:9000", Bucket: "b"}, true},
		{"缺端点", StorageConfig{Bucket: "b"}, false},
		{"缺桶", StorageConfig{Endpoint: "minio:9000"}, false},
		{"全空", StorageConfig{}, false},
		{"空白不算", StorageConfig{Endpoint: "  ", Bucket: " "}, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := item.config.Enabled(); got != item.want {
				t.Fatalf("Enabled() = %v，期望 %v", got, item.want)
			}
		})
	}
}

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"sessions":    "sessions/",
		"/sessions":   "sessions/",
		"sessions/":   "sessions/",
		"/sessions//": "sessions/",
		"a/b":         "a/b/",
	}
	for input, want := range cases {
		if got := normalizePrefix(input); got != want {
			t.Errorf("normalizePrefix(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestS3ObjectKey(t *testing.T) {
	key := sessionindex.Key{Host: "node-1", Project: "-tmp-work", Session: "s-1"}
	bare := &S3Store{prefix: ""}
	if got := bare.ObjectKey(key); got != "node-1/-tmp-work/s-1.jsonl" {
		t.Errorf("无前缀键 = %q", got)
	}
	prefixed := &S3Store{prefix: "sessions/"}
	if got := prefixed.ObjectKey(key); got != "sessions/node-1/-tmp-work/s-1.jsonl" {
		t.Errorf("带前缀键 = %q", got)
	}
}

// 未启用（client 为 nil）与非法键：Put/Get 都返回可读错误，不发网络请求。
func TestS3StoreDisabledAndInvalidKey(t *testing.T) {
	var store *S3Store
	if err := store.Put(t.Context(), sessionindex.Key{Host: "h", Project: "p", Session: "s"}, []byte("x")); err == nil {
		t.Error("未启用的 Put 应报错")
	}
	if _, _, err := store.Get(t.Context(), sessionindex.Key{Host: "h", Project: "p", Session: "s"}); err == nil {
		t.Error("未启用的 Get 应报错")
	}
	live := &S3Store{}
	if err := live.Put(t.Context(), sessionindex.Key{Host: "h", Project: "..", Session: "s"}, []byte("x")); err == nil {
		t.Error("非法键的 Put 应报错")
	}
	if _, _, err := live.Get(t.Context(), sessionindex.Key{Host: "h", Project: "..", Session: "s"}); err == nil {
		t.Error("非法键的 Get 应报错")
	}
}

func TestNewS3StoreMissingConfig(t *testing.T) {
	if _, err := NewS3Store(StorageConfig{}); err == nil {
		t.Error("缺 endpoint/bucket 应报错")
	}
	if _, err := NewS3Store(StorageConfig{Endpoint: "minio:9000"}); err == nil {
		t.Error("缺 bucket 应报错")
	}
}

// 端点前缀决定 Secure：https:// 打开、http:// 关闭。
func TestNewS3StoreEndpointScheme(t *testing.T) {
	store, err := NewS3Store(StorageConfig{Endpoint: "https://minio.example:9000", Bucket: "b"})
	if err != nil {
		t.Fatalf("https 端点：%v", err)
	}
	if got := store.ObjectKey(sessionindex.Key{Host: "h", Project: "p", Session: "s"}); got != "h/p/s.jsonl" {
		t.Errorf("键 = %q", got)
	}
	if _, err := NewS3Store(StorageConfig{Endpoint: "http://minio.example:9000", Bucket: "b", Prefix: "sessions"}); err != nil {
		t.Fatalf("http 端点：%v", err)
	}
}

// fakeS3 是一个进程内的极简 S3 端点，覆盖 minioObjectStore 的成功与错误路径。
// 它不是容器（CI 刻意不跑容器），只是一个 httptest 服务器：让 Put/Get/EnsureBucket
// 的真实 HTTP 往返能在单测里走到，真 MinIO 的端到端另见 test/e2e。
type fakeS3 struct {
	objects map[string][]byte
	// fail 非空时所有数据面请求返回它（测错误路径）
	status int
}

func newFakeS3(t *testing.T) (*httptest.Server, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 路径形如 /<bucket>/<object...>；桶级请求路径就是 /<bucket>
		trimmed := strings.TrimPrefix(request.URL.Path, "/")
		_, object, hasObject := strings.Cut(trimmed, "/")
		hasObject = hasObject && object != "" // 桶级请求（/<bucket>/）没有对象段
		if fake.status != 0 {
			writer.WriteHeader(fake.status)
			return
		}
		switch {
		case !hasObject && request.Method == http.MethodHead: // BucketExists
			writer.WriteHeader(http.StatusOK)
		case !hasObject && request.Method == http.MethodPut: // MakeBucket
			writer.WriteHeader(http.StatusOK)
		case hasObject && request.Method == http.MethodPut:
			body, _ := io.ReadAll(request.Body)
			fake.objects[object] = body
			writer.Header().Set("ETag", `"`+strings.Repeat("0", 32)+`"`)
			writer.WriteHeader(http.StatusOK)
		case hasObject && request.Method == http.MethodHead: // Stat
			body, ok := fake.objects[object]
			if !ok {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
			writer.Header().Set("ETag", `"`+strings.Repeat("0", 32)+`"`)
			writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			writer.WriteHeader(http.StatusOK)
		case hasObject && request.Method == http.MethodGet:
			body, ok := fake.objects[object]
			if !ok {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
			writer.Header().Set("ETag", `"`+strings.Repeat("0", 32)+`"`)
			writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			_, _ = writer.Write(body)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, fake
}

// 用假 S3 端点走完整的 Put→Get→EnsureBucket 往返。
func TestS3StoreRoundTripAgainstFakeS3(t *testing.T) {
	server, fake := newFakeS3(t)
	store, err := NewS3Store(StorageConfig{Endpoint: server.URL, Bucket: "assistant-sessions"})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	if err := store.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	key := sessionindex.Key{Host: "node-1", Project: "-tmp-work", Session: "s-1"}
	if err := store.Put(t.Context(), key, []byte("你好\n")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := string(fake.objects["node-1/-tmp-work/s-1.jsonl"]); got != "你好\n" {
		t.Fatalf("对象内容 = %q", got)
	}
	data, ok, err := store.Get(t.Context(), key)
	if err != nil || !ok || string(data) != "你好\n" {
		t.Fatalf("Get = %q ok=%v err=%v", data, ok, err)
	}
	// 不存在的对象：ok=false 且不是错误（HTTP 404 → NoSuchKey）
	if _, ok, err := store.Get(t.Context(), sessionindex.Key{Host: "h", Project: "p", Session: "缺失"}); ok || err != nil {
		t.Errorf("缺失对象 = ok=%v err=%v", ok, err)
	}
	// 服务端 403（权限不足）：Get/Put/EnsureBucket 都报错。
	// 用 403 而不是 500：minio-go 对 5xx 有指数退避重试，会让用例拖到十几秒；
	// 4xx 是终态，立即失败，同样覆盖错误包装路径。
	fake.status = http.StatusForbidden
	if err := store.Put(t.Context(), key, []byte("x")); err == nil {
		t.Error("403 时 Put 应报错")
	}
	if _, _, err := store.Get(t.Context(), key); err == nil {
		t.Error("403 时 Get 应报错")
	}
	if err := store.EnsureBucket(t.Context()); err == nil {
		t.Error("403 时 EnsureBucket 应报错")
	}
}

// NewS3Store 对坏端点报错（minio.New 的失败分支）。
func TestNewS3StoreBadEndpoint(t *testing.T) {
	if _, err := NewS3Store(StorageConfig{Endpoint: "http://[::1", Bucket: "b"}); err == nil {
		t.Error("坏端点应报错")
	}
}
