package sessionstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// StorageConfig 是会话归档的对象存储配置（S3 兼容：MinIO / 阿里云 OSS / AWS S3）。
//
// 整节缺省表示功能关闭——与 internal/statestore 的 Open("") 同一约定，调用方拿到
// 未启用的存储后全部操作安全跳过，而不是报错。
type StorageConfig struct {
	// Endpoint 是对象存储地址，形如 minio.internal:9000；带 http:// / https:// 前缀
	// 时前缀决定 Secure（前缀优先，便于照抄浏览器地址）。
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	// Secure 为真时走 HTTPS（Endpoint 无前缀时生效）
	Secure bool `json:"secure,omitempty"`
	// Region 是区域（S3 需要，MinIO 可留空）
	Region string `json:"region,omitempty"`
	// Prefix 是对象键前缀（桶内目录）；留空表示桶根
	Prefix string `json:"prefix,omitempty"`
}

// Enabled 判断配置是否完整到可以启用（端点与桶名都给了）。
func (c StorageConfig) Enabled() bool {
	return strings.TrimSpace(c.Endpoint) != "" && strings.TrimSpace(c.Bucket) != ""
}

// blobStore 是归档侧的抽象：只写与读回原始 jsonl，不做查询（查询走本地索引）。
//
// 抽成接口是为了让归档编排的逻辑测试不必起真对象存储：MinIO 属真实进程/网络，
// 与 Gitea 一样只能进 e2e（CI 刻意不跑容器）。
type blobStore interface {
	// Put 写入一条记录；已存在则覆盖（会话是只增的，但重传以最后一次为准）。
	Put(ctx context.Context, key sessionindex.Key, data []byte) error
	// Get 读回一条记录；不存在返回 ok=false。
	Get(ctx context.Context, key sessionindex.Key) ([]byte, bool, error)
}

// objectStore 是 S3Store 依赖的最小对象存储面（对象键 → 字节）。
//
// 再抽一层是因为 S3Store 上还有值得单测的逻辑：键校验、对象键布局、错误包装。
// 把真正碰网络的两三个调用收进 minioObjectStore，其余部分就能用假实现覆盖；
// 没有这一层，Put/Get 的每一行都只能靠 e2e 摸到。
type objectStore interface {
	put(ctx context.Context, object string, data []byte) error
	get(ctx context.Context, object string) ([]byte, bool, error)
	ensureBucket(ctx context.Context) error
}

// S3Store 是 S3 兼容对象存储上的归档目标。
type S3Store struct {
	store  objectStore
	prefix string
}

// NewS3Store 构造归档目标：校验配置、建立连接。桶的存在性由 EnsureBucket 单独确认
// （它在归档启动时调用一次），这样构造函数保持无副作用、可单测。
func NewS3Store(config StorageConfig) (*S3Store, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	bucket := strings.TrimSpace(config.Bucket)
	if endpoint == "" || bucket == "" {
		return nil, fmt.Errorf("对象存储配置缺少 endpoint 或 bucket")
	}
	secure := config.Secure
	if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint, secure = rest, true
	} else if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint, secure = rest, false
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(strings.TrimSpace(config.AccessKey), strings.TrimSpace(config.SecretKey), ""),
		Secure: secure,
		Region: strings.TrimSpace(config.Region),
	})
	if err != nil {
		return nil, fmt.Errorf("建立对象存储连接 %s: %w", endpoint, err)
	}
	return &S3Store{
		store:  minioObjectStore{client: client, bucket: bucket, region: strings.TrimSpace(config.Region)},
		prefix: normalizePrefix(config.Prefix),
	}, nil
}

// EnsureBucket 确保桶存在（不存在则创建空桶）。归档启动时调用一次，让「桶名写错」
// 在启动期就暴露，而不是等第一条会话归档才失败。
func (s *S3Store) EnsureBucket(ctx context.Context) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("对象存储未启用")
	}
	return s.store.ensureBucket(ctx)
}

// normalizePrefix 把前缀规整成"要么为空、要么以 / 结尾"，拼键时不用再判。
func normalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// ObjectKey 返回一条记录在桶里的对象键：<前缀><host>/<project>/<session>.jsonl。
//
// 与旧的磁盘记录库布局逐字相同，存量库可以整目录 rsync 上桶而不用改名。
func (s *S3Store) ObjectKey(key sessionindex.Key) string {
	return s.prefix + key.Host + "/" + key.Project + "/" + key.Session + ".jsonl"
}

// Put 写入一条记录（覆盖同键对象）。
func (s *S3Store) Put(ctx context.Context, key sessionindex.Key, data []byte) error {
	if !key.Valid() {
		return fmt.Errorf("非法的会话键：%q/%q/%q", key.Host, key.Project, key.Session)
	}
	if s == nil || s.store == nil {
		return fmt.Errorf("对象存储未启用")
	}
	if err := s.store.put(ctx, s.ObjectKey(key), data); err != nil {
		return fmt.Errorf("上传会话记录 %s: %w", key, err)
	}
	return nil
}

// Get 读回一条记录；对象不存在返回 ok=false（不是错误）。
func (s *S3Store) Get(ctx context.Context, key sessionindex.Key) ([]byte, bool, error) {
	if !key.Valid() {
		return nil, false, fmt.Errorf("非法的会话键：%q/%q/%q", key.Host, key.Project, key.Session)
	}
	if s == nil || s.store == nil {
		return nil, false, fmt.Errorf("对象存储未启用")
	}
	data, ok, err := s.store.get(ctx, s.ObjectKey(key))
	if err != nil {
		return nil, false, fmt.Errorf("读取会话记录 %s: %w", key, err)
	}
	return data, ok, nil
}

// minioObjectStore 是 objectStore 的 minio-go 实现：真正碰网络的那一层。
type minioObjectStore struct {
	client *minio.Client
	bucket string
	region string
}

func (m minioObjectStore) put(ctx context.Context, object string, data []byte) error {
	_, err := m.client.PutObject(ctx, m.bucket, object,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/x-ndjson; charset=utf-8"})
	return err
}

func (m minioObjectStore) get(ctx context.Context, object string) ([]byte, bool, error) {
	handle, err := m.client.GetObject(ctx, m.bucket, object, minio.GetObjectOptions{})
	if err != nil {
		return nil, false, err
	}
	defer handle.Close()
	// GetObject 是惰性的：对象不存在要到第一次读或 Stat 时才暴露。
	if _, err := handle.Stat(); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, err := io.ReadAll(handle)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (m minioObjectStore) ensureBucket(ctx context.Context) error {
	exists, err := m.client.BucketExists(ctx, m.bucket)
	if err != nil {
		return fmt.Errorf("检查桶 %s: %w", m.bucket, err)
	}
	if exists {
		return nil
	}
	if err := m.client.MakeBucket(ctx, m.bucket, minio.MakeBucketOptions{Region: m.region}); err != nil {
		return fmt.Errorf("创建桶 %s: %w", m.bucket, err)
	}
	return nil
}
