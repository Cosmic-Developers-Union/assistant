//go:build e2e

// 会话归档的端到端：真实 MinIO（test/minio/docker-compose.yaml，由 make test-e2e
// 起）+ 真实 SQLite 索引。没有 ASSISTANT_E2E_MINIO_* 时自动跳过。
//
// 这里覆盖的是单测碰不到的部分：真对象存储往返、桶的自动创建、以及「归档一条会话 →
// session_list 查到 → session_search 用中文子串命中 → session_read 分页 → 对象确实
// 落在 bucket 里」这条完整链路。
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"
)

type minioEnv struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
}

// minioEnvironment 解析 MinIO 连接信息（环境变量优先，否则读包目录下的 .env）。
func minioEnvironment(t *testing.T) minioEnv {
	t.Helper()
	env := minioEnv{
		Endpoint:  os.Getenv("ASSISTANT_E2E_MINIO_ENDPOINT"),
		Bucket:    os.Getenv("ASSISTANT_E2E_MINIO_BUCKET"),
		AccessKey: os.Getenv("ASSISTANT_E2E_MINIO_ACCESS_KEY"),
		SecretKey: os.Getenv("ASSISTANT_E2E_MINIO_SECRET_KEY"),
	}
	if env.Endpoint == "" {
		if data, err := os.ReadFile(".env"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
				if !ok {
					continue
				}
				switch key {
				case "ASSISTANT_E2E_MINIO_ENDPOINT":
					env.Endpoint = value
				case "ASSISTANT_E2E_MINIO_BUCKET":
					env.Bucket = value
				case "ASSISTANT_E2E_MINIO_ACCESS_KEY":
					env.AccessKey = value
				case "ASSISTANT_E2E_MINIO_SECRET_KEY":
					env.SecretKey = value
				}
			}
		}
	}
	if env.Endpoint == "" || env.Bucket == "" {
		t.Skip("缺少 ASSISTANT_E2E_MINIO_ENDPOINT / BUCKET（先运行 test/minio/up.sh）")
	}
	return env
}

// 一段真实的 claude 文本记录：中文子串、「待评审」等都在里面。
const archiveTranscript = `{"type":"user","message":{"role":"user","content":"你好，帮我查待评审队列"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"待评审的队列如下：a、b"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"a.jsonl\nb.jsonl"}]}}
`

// TestSessionArchiveRoundTrip 覆盖归档 → 索引查询 → 对象落桶的完整链路。
func TestSessionArchiveRoundTrip(t *testing.T) {
	env := minioEnvironment(t)
	sessionDir := filepath.Join(t.TempDir(), "claude")
	projectDir := filepath.Join(sessionDir, "projects", "-tmp-e2e")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "sess-e2e-1"
	if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(archiveTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	storage := sessionstore.StorageConfig{
		Endpoint:  env.Endpoint,
		Bucket:    env.Bucket,
		AccessKey: env.AccessKey,
		SecretKey: env.SecretKey,
	}
	blob, err := sessionstore.NewS3Store(storage)
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	// 桶可能不存在：EnsureBucket 应当建出来（MinIO 上真实执行 MakeBucket）
	if err := blob.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatalf("打开索引: %v", err)
	}
	defer index.Close()

	archive := sessionstore.NewArchiver(sessionstore.CollectOptions{
		Root: sessionDir,
		Host: "e2e-host",
	}, blob, index, t.Logf)
	if archive == nil {
		t.Fatal("归档器不该为 nil")
	}
	summary, err := archive.Archive(t.Context())
	if err != nil || summary.Stored != 1 {
		t.Fatalf("归档 = %+v err=%v", summary, err)
	}

	// session_list：索引里查得到
	records, err := index.List(sessionstore.Filter{}, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("索引列表 = %+v err=%v", records, err)
	}
	key := records[0].Key

	// session_search：中文子串命中（trigram 分词器的关键行为）
	hits, err := index.Search("待评审", sessionstore.Filter{}, 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("中文子串检索 = %+v err=%v", hits, err)
	}
	if !strings.Contains(hits[0].Text, "待评审") {
		t.Errorf("命中文本不含检索词：%q", hits[0].Text)
	}

	// session_read：分页读
	page, err := index.Read(key, 1, 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("分页读取 = %+v err=%v", page, err)
	}

	// 对象确实落在 bucket 里，且内容与本地记录一致
	payload, ok, err := blob.Get(t.Context(), key)
	if err != nil || !ok {
		t.Fatalf("对象未落桶：ok=%v err=%v", ok, err)
	}
	if string(payload) != archiveTranscript {
		t.Errorf("桶里对象内容与本地记录不一致：\n%s", payload)
	}
}

// TestSessionArchiveIncremental 覆盖增量：同一会话未变时不重传，变了才重传。
func TestSessionArchiveIncremental(t *testing.T) {
	env := minioEnvironment(t)
	sessionDir := filepath.Join(t.TempDir(), "claude")
	projectDir := filepath.Join(sessionDir, "projects", "-tmp-e2e")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, "sess-e2e-2.jsonl")
	if err := os.WriteFile(path, []byte(archiveTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	blob, err := sessionstore.NewS3Store(sessionstore.StorageConfig{
		Endpoint: env.Endpoint, Bucket: env.Bucket,
		AccessKey: env.AccessKey, SecretKey: env.SecretKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.EnsureBucket(t.Context()); err != nil {
		t.Fatal(err)
	}
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	archive := sessionstore.NewArchiver(sessionstore.CollectOptions{Root: sessionDir, Host: "h"}, blob, index, nil)

	first, err := archive.Archive(t.Context())
	if err != nil || first.Stored != 1 {
		t.Fatalf("首次 = %+v err=%v", first, err)
	}
	second, err := archive.Archive(t.Context())
	if err != nil || second.Skipped != 1 || second.Stored != 0 {
		t.Fatalf("二次应跳过 = %+v err=%v", second, err)
	}
	// 追加一行：水位变化 → 重传
	if err := os.WriteFile(path, []byte(archiveTranscript+`{"type":"user","message":{"role":"user","content":"追加"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := archive.Archive(context.Background())
	if err != nil || third.Stored != 1 {
		t.Fatalf("变化后应重传 = %+v err=%v", third, err)
	}
}
