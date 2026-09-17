// Package statestore 是基于 SQLite（WAL 模式）的共享状态库：daemon 运行态
// （目标、队列、会话、结果）的持久落点与跨进程内省窗口——任何能读到该文件
// 的进程（sqlite3 CLI、DBeaver、`assistant run --dry-run`）都能查询当前状态，
// 不必经过 daemon API。
//
// WAL 模式允许多读者并发一个写者：dispatcher 循环在会话生命周期内写状态，
// 状态 API 与外部工具只读。同一待办的全局互斥也落在这里（sqlite 事务保证
// 跨进程原子），配合进程内的 settled 守卫构成两级防重。
package statestore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store 是状态库句柄。零值不可用，须 Open。
type Store struct {
	db *sql.DB
}

// schema 是建表语句（幂等）。
const schema = `
CREATE TABLE IF NOT EXISTS targets (
	host         TEXT NOT NULL,
	repository   TEXT NOT NULL,
	dir          TEXT NOT NULL DEFAULT '',
	base_branch  TEXT NOT NULL DEFAULT '',
	managed      INTEGER NOT NULL DEFAULT 0,
	ready        INTEGER NOT NULL DEFAULT 1,
	skip_reason  TEXT NOT NULL DEFAULT '',
	provider     TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (host, repository)
);
CREATE TABLE IF NOT EXISTS queue (
	host       TEXT NOT NULL,
	repository TEXT NOT NULL,
	kind       TEXT NOT NULL,
	number     INTEGER NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	checked_at TEXT NOT NULL,
	PRIMARY KEY (host, repository, kind, number)
);
CREATE TABLE IF NOT EXISTS sessions (
	host        TEXT NOT NULL,
	repository  TEXT NOT NULL,
	kind        TEXT NOT NULL,
	number      INTEGER NOT NULL,
	state       TEXT NOT NULL,           -- running / success / error
	subtype     TEXT NOT NULL DEFAULT '',
	started_at  TEXT NOT NULL,
	finished_at TEXT,
	turns       INTEGER NOT NULL DEFAULT 0,
	cost_usd    REAL NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	session_id  TEXT NOT NULL DEFAULT '',
	errors      TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (host, repository, kind, number, started_at)
);
CREATE INDEX IF NOT EXISTS idx_sessions_running ON sessions(host, repository, kind, number) WHERE state = 'running';
`

// Open 打开（必要时创建）状态库并启用 WAL。path 为空返回 nil（未启用状态库，
// 调用方以 nil Store 安全跳过）。
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建状态库目录 %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("打开状态库 %s: %w", path, err)
	}
	// SQLite 单写者：串行化所有写连接，避免 SQLITE_BUSY
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化状态库 %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Target 是一个被监控目标（与 daemon.Target 同构）。
type Target struct {
	Host       string
	Repository string
	Dir        string
	BaseBranch string
	Managed    bool
	Ready      bool
	SkipReason string
	Provider   string
}

// UpsertTarget 登记目标（启动时与就绪状态变化时调用）。
func (s *Store) UpsertTarget(target Target) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO targets (host, repository, dir, base_branch, managed, ready, skip_reason, provider)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(host, repository) DO UPDATE SET
		   dir=excluded.dir, base_branch=excluded.base_branch, managed=excluded.managed,
		   ready=excluded.ready, skip_reason=excluded.skip_reason, provider=excluded.provider`,
		target.Host, target.Repository, target.Dir, target.BaseBranch,
		boolToInt(target.Managed), boolToInt(target.Ready), target.SkipReason, target.Provider)
	return err
}

// ReplaceQueue 以本轮检测结果整体替换队列（单事务）。
func (s *Store) ReplaceQueue(host, repository string, at time.Time, items []Item) error {
	if s == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM queue WHERE host = ? AND repository = ?`, host, repository); err != nil {
		return err
	}
	checked := at.UTC().Format(time.RFC3339Nano)
	for _, item := range items {
		if _, err := tx.Exec(
			`INSERT INTO queue (host, repository, kind, number, title, checked_at) VALUES (?, ?, ?, ?, ?, ?)`,
			host, repository, item.Kind, item.Number, item.Title, checked); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Item 是队列里的一个待办。
type Item struct {
	Kind   string
	Number int64
	Title  string
}

// StartSession 记录会话开始；全局互斥：同一 (host, repository, kind, number)
// 已有 running 会话时返回 false（同一待办同一时刻只允许一个 claude）。
// staleAfter 之前的残留 running 视为崩溃残留，直接接管。
func (s *Store) StartSession(host, repository string, item Item, at time.Time, sessionID string, staleAfter time.Duration) (bool, error) {
	if s == nil {
		return true, nil
	}
	// 崩溃残留回收：更早的 running 记录按超时收尾
	if _, err := s.db.Exec(
		`UPDATE sessions SET state = 'error', finished_at = ?, errors = '进程崩溃残留（超时接管）'
		 WHERE host = ? AND repository = ? AND kind = ? AND number = ? AND state = 'running'
		   AND started_at < ?`,
		at.UTC().Format(time.RFC3339Nano), host, repository, item.Kind, item.Number,
		at.Add(-staleAfter).UTC().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions
		 WHERE host = ? AND repository = ? AND kind = ? AND number = ? AND state = 'running'`,
		host, repository, item.Kind, item.Number).Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions (host, repository, kind, number, state, started_at, session_id)
		 VALUES (?, ?, ?, ?, 'running', ?, ?)`,
		host, repository, item.Kind, item.Number, at.UTC().Format(time.RFC3339Nano), sessionID)
	return err == nil, err
}

// FinishSession 记录会话结束（定位该待办当前唯一一条 running 记录）。
func (s *Store) FinishSession(host, repository string, item Item, startedAt time.Time, result Result) error {
	if s == nil {
		return nil
	}
	state := "success"
	if result.IsError {
		state = "error"
	}
	finishedAt := result.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}
	// startedAt 已知时精确匹配；零值时按 running 记录收尾（同待办至多一条）
	var query string
	var args []any
	if startedAt.IsZero() {
		query = `UPDATE sessions SET state = ?, subtype = ?, finished_at = ?, turns = ?, cost_usd = ?,
		    duration_ms = ?, session_id = ?, errors = ?
		 WHERE host = ? AND repository = ? AND kind = ? AND number = ? AND state = 'running'`
		args = []any{state, result.Subtype, finishedAt.UTC().Format(time.RFC3339Nano),
			result.Turns, result.CostUSD, result.DurationMS, result.SessionID,
			strings.Join(result.Errors, "\n"),
			host, repository, item.Kind, item.Number}
	} else {
		query = `UPDATE sessions SET state = ?, subtype = ?, finished_at = ?, turns = ?, cost_usd = ?,
		    duration_ms = ?, session_id = ?, errors = ?
		 WHERE host = ? AND repository = ? AND kind = ? AND number = ? AND started_at = ? AND state = 'running'`
		args = []any{state, result.Subtype, finishedAt.UTC().Format(time.RFC3339Nano),
			result.Turns, result.CostUSD, result.DurationMS, result.SessionID,
			strings.Join(result.Errors, "\n"),
			host, repository, item.Kind, item.Number, startedAt.UTC().Format(time.RFC3339Nano)}
	}
	_, err := s.db.Exec(query, args...)
	return err
}

// Result 是一次会话的归集结果（与 dispatcher.SessionOutcome 同构）。
type Result struct {
	Subtype    string
	IsError    bool
	Turns      int
	CostUSD    float64
	DurationMS int64
	SessionID  string
	Errors     []string
	// FinishedAt 由调用方传 time.Now()；零值时 FinishSession 内部补当前时间。
	FinishedAt time.Time
}

// Session 是 sessions 表的一行（内省视图）。
type Session struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Kind       string `json:"kind"`
	Number     int64  `json:"number"`
	State      string `json:"state"`
	Subtype    string `json:"subtype"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Turns      int    `json:"turns"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMS int64   `json:"duration_ms"`
	SessionID  string  `json:"session_id"`
	Errors     string  `json:"errors,omitempty"`
}

// TargetRow 是 targets 表的内省视图（小写 JSON 键，与其他 API 一致）。
type TargetRow struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Dir        string `json:"dir"`
	BaseBranch string `json:"base_branch"`
	Managed    bool   `json:"managed"`
	Ready      bool   `json:"ready"`
	SkipReason string `json:"skip_reason"`
	Provider   string `json:"provider"`
}

// RecentSessions 返回最近的会话记录（内省）。
func (s *Store) RecentSessions(limit int) ([]Session, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT host, repository, kind, number, state, subtype, started_at,
		        COALESCE(finished_at, ''), turns, cost_usd, duration_ms, session_id,
		        COALESCE(errors, '')
		 FROM sessions ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.Host, &session.Repository, &session.Kind, &session.Number,
			&session.State, &session.Subtype, &session.StartedAt, &session.FinishedAt,
			&session.Turns, &session.CostUSD, &session.DurationMS, &session.SessionID, &session.Errors); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// Snapshot 是一次完整内省。
type Snapshot struct {
	Targets  []TargetRow `json:"targets"`
	Queue    []QueueRow  `json:"queue"`
	Sessions []Session   `json:"recent_sessions"`
}

// QueueRow 是队列里的一行。
type QueueRow struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Kind       string `json:"kind"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	CheckedAt  string `json:"checked_at"`
}

// Snapshot 读取全量状态（内省）。
func (s *Store) Snapshot(sessionLimit int) (*Snapshot, error) {
	if s == nil {
		return nil, nil
	}
	snapshot := &Snapshot{}
	rows, err := s.db.Query(
		`SELECT host, repository, dir, base_branch, managed, ready, skip_reason, provider FROM targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var target Target
		var managed, ready int
		if err := rows.Scan(&target.Host, &target.Repository, &target.Dir, &target.BaseBranch,
			&managed, &ready, &target.SkipReason, &target.Provider); err != nil {
			return nil, err
		}
		target.Managed = managed == 1
		target.Ready = ready == 1
		snapshot.Targets = append(snapshot.Targets, TargetRow{
			Host: target.Host, Repository: target.Repository, Dir: target.Dir,
			BaseBranch: target.BaseBranch, Managed: target.Managed, Ready: target.Ready,
			SkipReason: target.SkipReason, Provider: target.Provider,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	queueRows, err := s.db.Query(
		`SELECT host, repository, kind, number, title, checked_at FROM queue ORDER BY host, repository, kind, number`)
	if err != nil {
		return nil, err
	}
	defer queueRows.Close()
	for queueRows.Next() {
		var row QueueRow
		if err := queueRows.Scan(&row.Host, &row.Repository, &row.Kind, &row.Number, &row.Title, &row.CheckedAt); err != nil {
			return nil, err
		}
		snapshot.Queue = append(snapshot.Queue, row)
	}
	if err := queueRows.Err(); err != nil {
		return nil, err
	}

	sessions, err := s.RecentSessions(sessionLimit)
	if err != nil {
		return nil, err
	}
	snapshot.Sessions = sessions
	return snapshot, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
