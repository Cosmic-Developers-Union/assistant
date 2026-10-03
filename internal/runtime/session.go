package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"uuid"
)

// LocalSession 让 Claude 的记录只落在运行根中，稳定项目名使 worktree 删除后仍可续接。
type LocalSession struct{ Root string }

// ID 使用事件锚点派生稳定 id；聊天按用户续接，评审 head 变化即重开。
func (s LocalSession) ID(ev Event) string {
	anchor := ev.Title
	if ev.Kind == "gitea-review" {
		anchor = ev.Head
	}
	if ev.Kind == "chat" {
		anchor = ev.User + "\x00" + ev.Title
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", ev.Host, ev.Repo, ev.Kind, ev.Number, anchor)))
	id := uuid.UUID(sum[:16])
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

// Dir 隔离每个会话的 Claude 配置根，路径不直接拼接平台输入。
func (s LocalSession) Dir(ctx context.Context, ev Event) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	host := fmt.Sprintf("%x", sha256.Sum256([]byte(ev.Host+"/"+ev.Repo)))
	dir := filepath.Join(s.Root, host[:16], s.ID(ev))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建会话目录: %w", err)
	}
	return dir, nil
}

// Sync 本地盘即终点，不另存一份 JSONL。
func (s LocalSession) Sync(context.Context, Event, string) error { return nil }

// Bucket 是远端会话副本的窄接口，恢复与上传都接受取消。
type Bucket interface {
	Fetch(context.Context, string) ([]byte, bool, error)
	Put(context.Context, string, []byte) error
}

// S3Session 在 Claude 写入本地后固化 projects 子树，含子代理记录。
type S3Session struct {
	Local  LocalSession
	Remote Bucket
}

// ID 复用本地锚点派生，换存储不换记忆。
func (s S3Session) ID(ev Event) string { return s.Local.ID(ev) }
func (s S3Session) objectKey(ev Event) string {
	sum := sha256.Sum256([]byte(ev.Host + "/" + ev.Repo))
	return fmt.Sprintf("%x/%s.tar.gz", sum[:16], s.ID(ev))
}

// Dir 在首次取用时恢复，读不到远端时拒绝创建失忆会话。
func (s S3Session) Dir(ctx context.Context, ev Event) (string, error) {
	dir, err := s.Local.Dir(ctx, ev)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(transcript(dir, s.ID(ev))); err == nil {
		return dir, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	data, ok, err := s.Remote.Fetch(ctx, s.objectKey(ev))
	if err != nil {
		return "", fmt.Errorf("拉回会话副本: %w", err)
	}
	if ok {
		// 解压到临时目录，失败不会留下半份记忆，也不覆盖已存在的本地记录。
		staging, err := os.MkdirTemp(dir, ".restore-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(staging)
		if err := unpack(data, staging); err != nil {
			return "", fmt.Errorf("恢复会话副本: %w", err)
		}
		if _, err := os.Stat(transcript(staging, s.ID(ev))); err != nil {
			return "", fmt.Errorf("远端副本缺少会话记录: %w", err)
		}
		if err := os.RemoveAll(filepath.Join(dir, "projects")); err != nil {
			return "", err
		}
		if err := os.Rename(filepath.Join(staging, "projects"), filepath.Join(dir, "projects")); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Sync 只上传 Claude 的记录，不包含 MCP 配置或认证信息；没有记录不覆盖远端。
func (s S3Session) Sync(ctx context.Context, ev Event, dir string) error {
	if _, err := os.Stat(transcript(dir, s.ID(ev))); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	data, err := pack(dir)
	if err != nil {
		return fmt.Errorf("打包会话副本: %w", err)
	}
	return s.Remote.Put(ctx, s.objectKey(ev), data)
}

func transcript(dir, id string) string {
	return filepath.Join(dir, "projects", "assistant", id+".jsonl")
}

func pack(dir string) ([]byte, error) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gzipWriter)
	err := filepath.WalkDir(filepath.Join(dir, "projects"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("会话记录含非常规文件")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if err := writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: info.Size()}); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})
	err = errors.Join(err, writer.Close(), gzipWriter.Close())
	return buffer.Bytes(), err
}

func unpack(data []byte, dir string) error {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	var total int64
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			n, err := io.Copy(io.Discard, io.LimitReader(reader, (1<<20)+1))
			if n > 1<<20 {
				return fmt.Errorf("会话归档尾部过大")
			}
			return err
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(header.Name)
		total += header.Size
		if header.Typeflag != tar.TypeReg || !filepath.IsLocal(name) || filepath.Clean(name) != name || !strings.HasPrefix(filepath.ToSlash(name), "projects/") || strings.Contains(name, "\\") || total > 256<<20 {
			return fmt.Errorf("非法或过大的会话归档条目")
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(file, archive, header.Size)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
	}
}

func safeHost(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:16])
}
