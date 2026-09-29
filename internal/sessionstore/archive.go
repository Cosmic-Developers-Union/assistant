package sessionstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
)

// Archiver 把本地 claude 会话归档到对象存储，并写进本地检索索引。
//
// 两条触发路径共用同一个 Archiver：对话桥每轮结束后按会话 id 即时归档一条，
// daemon 的后台巡检按拍全量跑一遍，捞评审/分诊会话以及即时归档漏掉的。
// 防重与增量都由索引里的水位（源文件大小 + 修改时间）承担，不另设状态文件。
type Archiver struct {
	options CollectOptions
	blob    blobStore
	index   *sessionindex.Store
	logf    func(string, ...any)
}

// Summary 是一次归档的统计。
type Summary struct {
	// Scanned 是扫描到的本地会话文件数
	Scanned int `json:"scanned"`
	// Stored 是本次真正归档（上传 + 建索引）的条数
	Stored int `json:"stored"`
	// Skipped 是水位未变、跳过的条数
	Skipped int `json:"skipped"`
	// Failed 是归档失败的条数
	Failed int `json:"failed"`
}

// NewArchiver 构造归档器。
//
// 对象存储或索引任一未启用（nil）时返回 nil——归档整体关闭，调用方按 nil 接收者
// 安全跳过，与 internal/statestore 的"未配置即停用"约定一致。
func NewArchiver(options CollectOptions, blob blobStore, index *sessionindex.Store, logf func(string, ...any)) *Archiver {
	if blob == nil || index == nil {
		return nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Archiver{options: options, blob: blob, index: index, logf: logf}
}

// Close 关闭归档器持有的索引句柄。归档器不持有对象存储的关闭语义（连接由
// minio 客户端池化，进程退出自然释放），只负责索引文件。
func (a *Archiver) Close() error {
	if a == nil {
		return nil
	}
	return a.index.Close()
}

// Archive 全量扫描并归档所有变过的会话。
//
// 单条失败不中断整轮（记录条数并继续），最终返回第一个错误供调用方记日志：
// 后台巡检里一条坏记录不该挡住其余会话的归档。
func (a *Archiver) Archive(ctx context.Context) (Summary, error) {
	if a == nil {
		return Summary{}, nil
	}
	candidates, err := Scan(a.options)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{Scanned: len(candidates)}
	var firstErr error
	for _, candidate := range candidates {
		stored, err := a.archiveCandidate(ctx, candidate)
		switch {
		case err != nil:
			summary.Failed++
			a.logf("归档会话 %s 失败：%v", candidate.Key, err)
			if firstErr == nil {
				firstErr = err
			}
		case stored:
			summary.Stored++
		default:
			summary.Skipped++
		}
	}
	return summary, firstErr
}

// ArchiveSession 归档单条会话（按 claude 会话 id 定位）；找不到返回 ok=false。
//
// 供对话桥每轮结束后调用：同一轮里刚聊完的内容必须马上可查，否则这个功能就失去了
// 存在理由（agent 上下文被压缩后回查的正是刚聊过的内容）。
func (a *Archiver) ArchiveSession(ctx context.Context, sessionID string) (bool, error) {
	if a == nil {
		return false, nil
	}
	candidate, ok, err := Find(a.options, sessionID)
	if err != nil || !ok {
		return false, err
	}
	return a.archiveCandidate(ctx, candidate)
}

// archiveCandidate 归档单条候选：水位未变则跳过，否则读文件、上传、建索引。
func (a *Archiver) archiveCandidate(ctx context.Context, candidate Candidate) (bool, error) {
	size, modTime, ok, err := a.index.Watermark(candidate.Key)
	if err != nil {
		return false, err
	}
	if ok && size == candidate.Size && modTime == candidate.ModTime {
		return false, nil
	}
	lines, err := readLines(candidate.Path)
	if err != nil {
		return false, fmt.Errorf("读取会话记录 %s: %w", candidate.Path, err)
	}
	payload := joinLines(lines)
	if err := a.blob.Put(ctx, candidate.Key, payload); err != nil {
		return false, err
	}
	record, messages := a.build(candidate, lines, len(payload))
	if err := a.index.Upsert(record, messages); err != nil {
		return false, err
	}
	a.logf("已归档会话 %s（%d 行，来源 %s）", candidate.Key, len(lines), record.Source)
	return true, nil
}

// build 由候选与原始行组装出索引记录与消息列表。
func (a *Archiver) build(candidate Candidate, lines []string, bytes int) (sessionindex.Record, []sessionindex.Message) {
	modTime, err := time.Parse(time.RFC3339Nano, candidate.ModTime)
	if err != nil {
		modTime = time.Now()
	}
	record := sessionindex.Record{
		Key:        candidate.Key,
		Source:     DetectSource(candidate.Key.Project),
		Lines:      len(lines),
		Bytes:      bytes,
		CreatedAt:  modTime.UTC().Format(time.RFC3339),
		UpdatedAt:  modTime.UTC().Format(time.RFC3339),
		ArchivedAt: time.Now().UTC().Format(time.RFC3339),
		Size:       candidate.Size,
		ModTime:    candidate.ModTime,
	}
	if chat, ok := readChatSessions(a.options.ChatDir)[candidate.Key.Session]; ok {
		record.Source = "chat"
		record.Conversation = chat.ConversationID
		record.Transport = chat.Transport
		record.Title = chat.Title
		record.Model = chat.Model
	}
	record.FirstUserText, record.LastAssistantText = Summarize(lines)
	return record, extractAll(lines)
}

// extractAll 把原始 jsonl 行摊平成可读消息（一行的 content 数组可能含多条）。
func extractAll(lines []string) []sessionindex.Message {
	messages := make([]sessionindex.Message, 0, len(lines))
	for index, line := range lines {
		messages = append(messages, ExtractMessages([]byte(line), index)...)
	}
	return messages
}

// joinLines 把原始行拼回 jsonl 载荷（每行以换行结尾，与磁盘上的记录同格式）。
func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
