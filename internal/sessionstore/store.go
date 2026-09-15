// Package sessionstore 是会话记录的远端存储与查询层：一次会话一条 jsonl（一行一个
// 事件，与 claude 的文本记录同格式），以 (host, project, session) 为键，附带一份
// meta.json（来源、会话实体、时间、行数）用于检索。
//
// 记录是**原样的 jsonl**：服务端不解释语义，只在查询时按需抽取可读文本（user /
// assistant / thinking / tool / error），所以上下文被压缩后 agent 仍能回查完整历史。
package sessionstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Key 是一条会话记录的键：哪台宿主机、哪个项目（claude 的项目目录名）、哪个会话。
type Key struct {
	Host    string `json:"host"`
	Project string `json:"project"`
	Session string `json:"session"`
}

func (k Key) String() string { return k.Host + "/" + k.Project + "/" + k.Session }

// Valid 判断键是否完整（三段都非空且不含路径分隔符）。
func (k Key) Valid() bool {
	for _, part := range []string{k.Host, k.Project, k.Session} {
		if strings.TrimSpace(part) == "" || strings.ContainsAny(part, `/\`) {
			return false
		}
	}
	return true
}

// Meta 是一条会话记录的元数据（可检索字段）。
type Meta struct {
	Key
	// Source 是会话用途：review / triage / chat / unknown
	Source string `json:"source,omitempty"`
	// Conversation 是聊天会话的会话实体（见 internal/conversations 的映射层）；
	// 非聊天会话为空
	Conversation string `json:"conversation,omitempty"`
	// Transport 是聊天会话的来源通道（weixin / …）
	Transport string `json:"transport,omitempty"`
	Title     string `json:"title,omitempty"`
	Model     string `json:"model,omitempty"`
	Lines     int    `json:"lines"`
	Bytes     int    `json:"bytes"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	PushedAt  string `json:"pushed_at"`
	// FirstUserText / LastAssistantText 是摘要用的首尾片段（列表/检索时免读全文）
	FirstUserText     string `json:"first_user_text,omitempty"`
	LastAssistantText string `json:"last_assistant_text,omitempty"`
}

// Session 是一次会话记录的上行载荷：元数据 + 原始 jsonl 行。
type Session struct {
	Meta
	Lines []string `json:"lines"`
}

// Batch 是推送的一批会话。
type Batch struct {
	Sessions []Session `json:"sessions"`
}

// Message 是从记录里抽取出来的一条可读消息。
type Message struct {
	Line      int    `json:"line"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// Match 是一条检索命中。
type Match struct {
	Key
	Conversation string `json:"conversation,omitempty"`
	Source       string `json:"source,omitempty"`
	Transport    string `json:"transport,omitempty"`
	Message
}

// Filter 是列表/检索的过滤条件（零值表示不过滤）。
type Filter struct {
	Host         string
	Project      string
	Session      string
	Conversation string
	Source       string
	Since        time.Time
}

func (f Filter) match(meta Meta) bool {
	if f.Host != "" && meta.Host != f.Host {
		return false
	}
	if f.Project != "" && meta.Project != f.Project {
		return false
	}
	if f.Session != "" && meta.Session != f.Session {
		return false
	}
	if f.Conversation != "" && meta.Conversation != f.Conversation {
		return false
	}
	if f.Source != "" && meta.Source != f.Source {
		return false
	}
	if !f.Since.IsZero() {
		updated, err := time.Parse(time.RFC3339, meta.UpdatedAt)
		if err != nil || updated.Before(f.Since) {
			return false
		}
	}
	return true
}

// Store 是落在本地目录里的记录库。
type Store struct {
	root string
}

// NewStore 打开（必要时创建）记录库根目录。
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("缺少记录库目录")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// Root 返回记录库根目录。
func (s *Store) Root() string { return s.root }

func (s *Store) sessionPath(key Key) string {
	return filepath.Join(s.root, key.Host, key.Project, key.Session+".jsonl")
}

func (s *Store) metaPath(key Key) string {
	return filepath.Join(s.root, key.Host, key.Project, key.Session+".meta.json")
}

// Put 写入一条会话记录：先写数据再写元数据（元数据是"已完整落库"的标志）。
func (s *Store) Put(session Session) (Meta, error) {
	if !session.Key.Valid() {
		return Meta{}, fmt.Errorf("非法的会话键：%q/%q/%q", session.Host, session.Project, session.Session)
	}
	directory := filepath.Dir(s.sessionPath(session.Key))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return Meta{}, err
	}
	var payload strings.Builder
	for _, line := range session.Lines {
		payload.WriteString(strings.TrimRight(line, "\r\n"))
		payload.WriteByte('\n')
	}
	if err := os.WriteFile(s.sessionPath(session.Key), []byte(payload.String()), 0o644); err != nil {
		return Meta{}, err
	}
	meta := session.Meta
	meta.Lines = len(session.Lines)
	meta.Bytes = payload.Len()
	meta.PushedAt = time.Now().UTC().Format(time.RFC3339)
	if meta.UpdatedAt == "" {
		meta.UpdatedAt = meta.PushedAt
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if err := os.WriteFile(s.metaPath(session.Key), append(encoded, '\n'), 0o644); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// List 返回符合条件的会话元数据（按更新时间倒序）。
func (s *Store) List(filter Filter, limit int) ([]Meta, error) {
	metas := make([]Meta, 0, 16)
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // 元数据读不到就跳过（不因为一条坏记录让整个列表失败）
		}
		var meta Meta
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}
		if meta.Session == "" {
			meta.Session = strings.TrimSuffix(entry.Name(), ".meta.json")
		}
		if filter.match(meta) {
			metas = append(metas, meta)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].UpdatedAt != metas[j].UpdatedAt {
			return metas[i].UpdatedAt > metas[j].UpdatedAt
		}
		return metas[i].Key.String() < metas[j].Key.String()
	})
	if limit > 0 && len(metas) > limit {
		metas = metas[:limit]
	}
	return metas, nil
}

// Conversations 返回记录库里的会话实体（去重）及各自的记录条数。
func (s *Store) Conversations() (map[string]int, error) {
	metas, err := s.List(Filter{}, 0)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, meta := range metas {
		if meta.Conversation != "" {
			counts[meta.Conversation]++
		}
	}
	return counts, nil
}

// Read 读取一条记录的可读消息（offset/limit 按消息计；limit<=0 表示不限制）。
func (s *Store) Read(key Key, offset, limit int) ([]Message, error) {
	if !key.Valid() {
		return nil, fmt.Errorf("非法的会话键：%q/%q/%q", key.Host, key.Project, key.Session)
	}
	file, err := os.Open(s.sessionPath(key))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	messages := make([]Message, 0, 64)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		for _, message := range ExtractMessages(scanner.Bytes(), line) {
			if offset > 0 {
				offset--
				continue
			}
			messages = append(messages, message)
			if limit > 0 && len(messages) >= limit {
				return messages, nil
			}
		}
		line++
	}
	return messages, scanner.Err()
}

// Search 在记录里做子串检索（大小写不敏感），返回命中消息。
func (s *Store) Search(query string, filter Filter, limit int) ([]Match, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil, fmt.Errorf("检索词为空")
	}
	if limit <= 0 {
		limit = 50
	}
	metas, err := s.List(filter, 0)
	if err != nil {
		return nil, err
	}
	matches := make([]Match, 0, limit)
	for _, meta := range metas {
		messages, err := s.Read(meta.Key, 0, 0)
		if err != nil {
			continue
		}
		for _, message := range messages {
			if !strings.Contains(strings.ToLower(message.Text), needle) {
				continue
			}
			matches = append(matches, Match{
				Key:          meta.Key,
				Conversation: meta.Conversation,
				Source:       meta.Source,
				Transport:    meta.Transport,
				Message:      message,
			})
			if len(matches) >= limit {
				return matches, nil
			}
		}
	}
	return matches, nil
}
