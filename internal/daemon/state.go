// Package daemon 是 assistant run 的运行态与对话能力：
//
//   - Store 记录调度运行态（目标、待办队列、活跃会话、最近结果）；
//   - Serve 暴露只读 HTTP API（127.0.0.1 + Bearer token），并把端点写入
//     <配置目录>/daemon.json，`assistant mcp daemon` 靠它自举发现；
//   - Chat 把微信消息桥到稳定的 claude 会话（见 chat.go）。
//
// 状态只读：对话会话通过 daemon MCP 查询，不直接改调度行为。
package daemon

import (
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Target 是被调度的仓库。
type Target struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Dir        string `json:"dir"`
	BaseBranch string `json:"base_branch"`
	Managed    bool   `json:"managed"`
}

// Item 是一个待办。
type Item struct {
	Kind   string `json:"kind"`
	Number int64  `json:"number"`
	Title  string `json:"title"`
}

// Session 是一个进行中的评审/分诊会话。
type Session struct {
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
	Kind       string    `json:"kind"`
	Number     int64     `json:"number"`
	Title      string    `json:"title"`
	StartedAt  time.Time `json:"started_at"`
}

// Result 是一个已结束会话的归集结果。
type Result struct {
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
	Kind       string    `json:"kind"`
	Number     int64     `json:"number"`
	Title      string    `json:"title"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Subtype    string    `json:"subtype"`
	IsError    bool      `json:"is_error"`
	NumTurns   int       `json:"num_turns"`
	CostUSD    float64   `json:"cost_usd"`
	DurationMS int64     `json:"duration_ms"`
	SessionID  string    `json:"session_id,omitempty"`
	Errors     []string  `json:"errors,omitempty"`
}

// Queue 是某仓库最近一次检测到的待办清单。
type Queue struct {
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
	Items      []Item    `json:"items"`
	CheckedAt  time.Time `json:"checked_at"`
}

// Status 是 daemon 的完整只读快照。
type Status struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Now       time.Time `json:"now"`
	Targets   []Target  `json:"targets"`
	Queue     []Queue   `json:"queue"`
	Sessions  []Session `json:"sessions"`
	Recent    []Result  `json:"recent"`
}

// recentLimit 是最近结果归档的容量（超出丢弃最旧）。
const recentLimit = 100

// Store 是并发安全的运行态存储：每个仓库的调度循环写入，HTTP API 读取。
type Store struct {
	mu        sync.Mutex
	version   string
	pid       int
	startedAt time.Time
	targets   map[string]Target
	order     []string
	queue     map[string]Queue
	sessions  map[string]Session
	recent    []Result
}

// NewStore 创建运行态存储（version 是二进制版本，用于状态展示）。
func NewStore(version string) *Store {
	return &Store{
		version:   version,
		pid:       os.Getpid(),
		startedAt: time.Now(),
		targets:   map[string]Target{},
		queue:     map[string]Queue{},
		sessions:  map[string]Session{},
	}
}

func key(host, repository string) string {
	return host + "|" + repository
}

func itemKey(host, repository, kind string, number int64) string {
	return key(host, repository) + "|" + kind + "|" + strconv.FormatInt(number, 10)
}

// AddTarget 登记一个调度目标（运行期不变）。
func (s *Store) AddTarget(target Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repositoryKey := key(target.Host, target.Repository)
	if _, exists := s.targets[repositoryKey]; !exists {
		s.order = append(s.order, repositoryKey)
	}
	s.targets[repositoryKey] = target
}

// SetQueue 记录某仓库最近一轮检测到的待办。
func (s *Store) SetQueue(host, repository string, at time.Time, items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if items == nil {
		items = []Item{}
	}
	s.queue[key(host, repository)] = Queue{
		Host:       host,
		Repository: repository,
		Items:      slices.Clone(items),
		CheckedAt:  at,
	}
}

// Start 把一个待办标记为进行中。
func (s *Store) Start(host, repository string, item Item, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[itemKey(host, repository, item.Kind, item.Number)] = Session{
		Host:       host,
		Repository: repository,
		Kind:       item.Kind,
		Number:     item.Number,
		Title:      item.Title,
		StartedAt:  at,
	}
}

// Finish 结束一个待办并归档结果（开始时间缺省取 Start 记录的值）。
func (s *Store) Finish(host, repository string, item Item, result Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionKey := itemKey(host, repository, item.Kind, item.Number)
	if session, ok := s.sessions[sessionKey]; ok && result.StartedAt.IsZero() {
		result.StartedAt = session.StartedAt
	}
	delete(s.sessions, sessionKey)
	result.Host = host
	result.Repository = repository
	result.Kind = item.Kind
	result.Number = item.Number
	result.Title = item.Title
	if result.FinishedAt.IsZero() {
		result.FinishedAt = time.Now()
	}
	s.recent = append([]Result{result}, s.recent...)
	if len(s.recent) > recentLimit {
		s.recent = s.recent[:recentLimit]
	}
}

// Snapshot 返回当前完整状态（深拷贝，调用方可安全序列化）。
func (s *Store) Snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{
		Version:   s.version,
		PID:       s.pid,
		StartedAt: s.startedAt,
		Now:       time.Now(),
	}
	for _, repositoryKey := range s.order {
		status.Targets = append(status.Targets, s.targets[repositoryKey])
	}
	for _, repositoryKey := range s.order {
		queue, ok := s.queue[repositoryKey]
		if !ok {
			continue
		}
		status.Queue = append(status.Queue, queue)
	}
	for _, session := range s.sessions {
		status.Sessions = append(status.Sessions, session)
	}
	slices.SortFunc(status.Sessions, func(a, b Session) int {
		if a.StartedAt.Equal(b.StartedAt) {
			return 0
		}
		if a.StartedAt.Before(b.StartedAt) {
			return -1
		}
		return 1
	})
	status.Recent = slices.Clone(s.recent)
	return status
}
