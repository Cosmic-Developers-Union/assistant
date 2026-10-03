package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type memoryBucket struct {
	data             map[string][]byte
	fetchErr, putErr error
	fetches, puts    int
}

func (b *memoryBucket) Fetch(_ context.Context, key string) ([]byte, bool, error) {
	b.fetches++
	data, ok := b.data[key]
	return data, ok, b.fetchErr
}
func (b *memoryBucket) Put(_ context.Context, key string, data []byte) error {
	b.puts++
	if b.putErr != nil {
		return b.putErr
	}
	b.data[key] = bytes.Clone(data)
	return nil
}
func writeTranscript(t *testing.T, dir, id string) {
	t.Helper()
	path := transcript(dir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"message\":\"你好\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
func TestSessionAnchorsAndIsolation(t *testing.T) {
	local := LocalSession{Root: t.TempDir()}
	ev := testEvent()
	id := local.ID(ev)
	if local.ID(ev) != id {
		t.Fatal("id 不稳定")
	}
	ev.Head = "new"
	if local.ID(ev) == id {
		t.Fatal("新 head 沿用了旧会话")
	}
	ev.Kind = "triage"
	ev.Title = "title"
	id = local.ID(ev)
	ev.Head = "another"
	if local.ID(ev) != id {
		t.Fatal("Issue id 不按标题派生")
	}
	ev.Title = "new"
	if local.ID(ev) == id {
		t.Fatal("新标题没有新 id")
	}
	ev.Kind = "chat"
	ev.User = "user"
	id = local.ID(ev)
	ev.Title = "reset"
	if local.ID(ev) == id {
		t.Fatal("重置未改变记忆")
	}
	ev.Host = "../../host"
	ev.Repo = "../../../repo"
	dir, err := local.Dir(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(local.Root, dir)
	if err != nil || !filepath.IsLocal(relative) {
		t.Fatal("平台输入逃出会话根")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := local.Dir(ctx, ev); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := local.Sync(t.Context(), ev, dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := (LocalSession{Root: file}).Dir(t.Context(), ev); err == nil {
		t.Fatal("错误目录被接受")
	}
}
func TestS3RoundTripAndFailures(t *testing.T) {
	remote := &memoryBucket{data: map[string][]byte{}}
	session := S3Session{Local: LocalSession{Root: t.TempDir()}, Remote: remote}
	ev := testEvent()
	dir, err := session.Dir(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID(ev) != session.Local.ID(ev) {
		t.Fatal("存储改变 id")
	}
	if err := session.Sync(t.Context(), ev, dir); err != nil {
		t.Fatal(err)
	}
	if remote.puts != 0 {
		t.Fatal("空记忆覆盖远端")
	}
	writeTranscript(t, dir, session.ID(ev))
	// 配置中含密钥，副本只含 projects。
	_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("private-token"), 0o600)
	child := filepath.Join(dir, "projects", "assistant", session.ID(ev), "subagents", "agent.jsonl")
	_ = os.MkdirAll(filepath.Dir(child), 0o700)
	_ = os.WriteFile(child, []byte("子代理"), 0o600)
	if err := session.Sync(t.Context(), ev, dir); err != nil {
		t.Fatal(err)
	}
	restored := S3Session{Local: LocalSession{Root: t.TempDir()}, Remote: remote}
	other, err := restored.Dir(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(transcript(other, session.ID(ev))); err != nil || !bytes.Contains(data, []byte("你好")) {
		t.Fatalf("记录丢失 %s %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(other, ".mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("密钥配置进入副本")
	}
	fetches := remote.fetches
	if _, err := restored.Dir(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if fetches != remote.fetches {
		t.Fatal("覆盖了本地已有记忆")
	}
	sentinel := errors.New("对象存储离线")
	remote.fetchErr = sentinel
	fresh := S3Session{Local: LocalSession{Root: t.TempDir()}, Remote: remote}
	if _, err := fresh.Dir(t.Context(), ev); !errors.Is(err, sentinel) {
		t.Fatal("恢复失败被当成新会话", err)
	}
	remote.fetchErr = nil
	remote.putErr = sentinel
	if err := session.Sync(t.Context(), ev, dir); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	remote.putErr = nil
	remote.data[session.objectKey(ev)] = []byte("corrupt")
	if _, err := fresh.Dir(t.Context(), ev); err == nil {
		t.Fatal("损坏副本被接受")
	}
	_ = os.Symlink("/tmp", filepath.Join(dir, "projects", "link"))
	if err := session.Sync(t.Context(), ev, dir); err == nil {
		t.Fatal("归档符号链接被接受")
	}
}
func archiveFor(t *testing.T, name string, kind byte, size int64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gzipWriter)
	if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Mode: 0o600, Size: size}); err != nil {
		t.Fatal(err)
	}
	if size == 1 {
		_, _ = writer.Write([]byte("x"))
	}
	_ = writer.Close()
	_ = gzipWriter.Close()
	return buffer.Bytes()
}
func TestArchiveRejectsUntrustedPaths(t *testing.T) {
	for _, name := range []string{"../outside", "/tmp/outside", "settings.json", "projects/../../outside", "projects/evil\\path"} {
		if err := unpack(archiveFor(t, name, tar.TypeReg, 1), t.TempDir()); err == nil {
			t.Errorf("危险路径 %s 被接受", name)
		}
	}
	if err := unpack(archiveFor(t, "projects/link", tar.TypeSymlink, 0), t.TempDir()); err == nil {
		t.Fatal("链接被接受")
	}
	if err := unpack(archiveFor(t, "projects/huge", tar.TypeReg, 257<<20), t.TempDir()); err == nil {
		t.Fatal("超限副本被接受")
	}
	if _, err := pack(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("不存在的记录被接受")
	}
	remote := &memoryBucket{data: map[string][]byte{}}
	session := S3Session{Local: LocalSession{Root: t.TempDir()}, Remote: remote}
	ev := testEvent()
	remote.data[session.objectKey(ev)] = archiveFor(t, "projects/other.jsonl", tar.TypeReg, 1)
	if _, err := session.Dir(t.Context(), ev); err == nil {
		t.Fatal("错误 id 的记录被接受")
	}
}

func TestArchiveVerifiesChecksumAndBoundsTrailingData(t *testing.T) {
	good := archiveFor(t, "projects/assistant/id.jsonl", tar.TypeReg, 1)
	bad := append([]byte(nil), good...)
	bad[len(bad)-8] ^= 1
	if err := unpack(bad, t.TempDir()); err == nil {
		t.Fatal("损坏 gzip 校验和被接受")
	}
	reader, err := gzip.NewReader(bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(data)
	_, _ = writer.Write(make([]byte, 2<<20))
	_ = writer.Close()
	if err := unpack(buffer.Bytes(), t.TempDir()); err == nil {
		t.Fatal("无限尾部未被拒绝")
	}
}
