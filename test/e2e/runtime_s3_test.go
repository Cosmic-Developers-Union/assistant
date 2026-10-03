//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/Cosmic-Developers-Union/assistant/internal/runtime"
	"github.com/minio/minio-go/v7"
)

// TestSessionS3RoundTrip 用独立测试桶验证真实对象存储中的主会话和子代理恢复。
func TestSessionS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("ASSISTANT_E2E_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("未配置 MinIO 测试端点")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	bucketName := "assistant-test-" + uuid.New().String()
	bucket, err := runtime.NewS3Bucket(runtime.SessionConfig{Endpoint: "http://" + endpoint, Bucket: bucketName, AccessKey: os.Getenv("ASSISTANT_E2E_MINIO_ACCESS_KEY"), SecretKey: os.Getenv("ASSISTANT_E2E_MINIO_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	if err := bucket.Client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for object := range bucket.Client.ListObjects(cleanup, bucketName, minio.ListObjectsOptions{Recursive: true}) {
			if object.Err != nil {
				t.Error(object.Err)
				break
			}
			if err := bucket.Client.RemoveObject(cleanup, bucketName, object.Key, minio.RemoveObjectOptions{}); err != nil {
				t.Error(err)
			}
		}
		if err := bucket.Client.RemoveBucket(cleanup, bucketName); err != nil {
			t.Error(err)
		}
	})
	event := runtime.Event{Kind: "gitea-review", Host: "https://test.local", Repo: "team/project", Number: 42, Head: "current-head"}
	first := runtime.S3Session{Local: runtime.LocalSession{Root: t.TempDir()}, Remote: bucket}
	dir, err := first.Dir(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	id := first.ID(event)
	files := map[string]string{filepath.Join("projects", "assistant", id+".jsonl"): "主会话\n", filepath.Join("projects", "assistant", id, "subagents", "agent-1.jsonl"): "子代理\n"}
	for name, data := range files {
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Sync(ctx, event, dir); err != nil {
		t.Fatal(err)
	}
	second := runtime.S3Session{Local: runtime.LocalSession{Root: t.TempDir()}, Remote: bucket}
	restored, err := second.Dir(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID(event) != id {
		t.Fatal("更换本地根改变了会话 id")
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil || string(got) != want {
			t.Fatalf("恢复 %s: %q, %v", name, got, err)
		}
	}
}
