// Package sessionindex 是会话记录的**本地检索索引**：SQLite（WAL）里一份
// 元数据表 + 一份消息全文索引，供 `assistant mcp sessions` 回查已归档的聊天/
// 评审历史。
//
// 分工边界要说清楚：索引**不是**记录本体。记录本体是原样的 jsonl，归档在
// S3 对象存储里；索引是本机派生出来的加速结构——丢了可以由归档流程按本地
// claude 记录重建，反过来（记录还在、索引丢了）则由下一次归档补齐。所以
// 这里不做任何"权威数据"的假设，也不假设索引与 S3 时刻一致。
//
// 与 internal/statestore 同源：纯 Go 驱动（modernc.org/sqlite，CGO_ENABLED=0
// 下可用）、WAL 单写者、Open("") 返回 nil 表示未启用、所有方法 nil 接收者安全。
package sessionindex

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Key 是一条会话记录的键：哪台宿主机、哪个项目、哪个会话。
type Key struct {
	Host    string `json:"host"`
	Project string `json:"project"`
	Session string `json:"session"`
}

func (k Key) String() string { return k.Host + "/" + k.Project + "/" + k.Session }

// Valid 判断键是否可用：三段都非空，且不含路径分隔符或 "." / ".."。
//
// 拒绝点号段不是洁癖：Key 会被拼进对象存储的 key 与本地文件路径，而
// filepath.Join 会 Clean 掉 ".."，只挡斜杠挡不住它——三段都是 ".." 时键合法
// 却能把数据写到库根之外（这是旧实现里已实测的穿越）。
func (k Key) Valid() bool {
	for _, part := range []string{k.Host, k.Project, k.Session} {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || trimmed == "." || trimmed == ".." {
			return false
		}
		if strings.ContainsAny(part, `/\`) {
			return false
		}
	}
	return true
}

// Record 是一条已归档会话的元数据（可检索字段）。
type Record struct {
	Key
	// Source 是会话用途：review / triage / chat / unknown
	Source string `json:"source,omitempty"`
	// Conversation 是聊天会话的会话实体；非聊天会话为空
	Conversation string `json:"conversation,omitempty"`
	// Transport 是聊天会话的来源通道（weixin / …）
	Transport string `json:"transport,omitempty"`
	Title     string `json:"title,omitempty"`
	Model     string `json:"model,omitempty"`
	Lines     int    `json:"lines"`
	Bytes     int    `json:"bytes"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// ArchivedAt 是这条记录最后一次写入索引的时间
	ArchivedAt string `json:"archived_at,omitempty"`
	// Size / ModTime 是归档时**本地源文件**的大小与修改时间，即增量水位：
	// 归档流程据此判断会话是否变过（变了才重新读、重新上传、重新索引）。
	// 存在索引里而不是单独的增量状态文件，是为了让"是否已归档"只有一个事实来源。
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time,omitempty"`
	// FirstUserText / LastAssistantText 是摘要片段（列表时免读全文）
	FirstUserText     string `json:"first_user_text,omitempty"`
	LastAssistantText string `json:"last_assistant_text,omitempty"`
}

// Message 是从记录里抽取出来的一条可读消息。
type Message struct {
	Line      int    `json:"line"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// Hit 是一条检索命中。
type Hit struct {
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
	Conversation string
	Source       string
	Since        time.Time
}

// match 返回该过滤条件作用在一条消息所属记录上的 SQL 片段与参数。
//
// 拆成 SQL 片段而不是"取出来再判"：Search 与 List 都要按记录过滤，
// 让过滤下推到 SQL 才能用上索引，而不是把全部命中拉进内存再筛。
//
// prefix 是列所属表名（Search 的 join 里 sessions 与 messages 列名会撞车，
// 必须限定；List 只查 sessions，传空即可）。
func (f Filter) match(prefix string) (string, []any) {
	conditions := make([]string, 0, 5)
	arguments := make([]any, 0, 5)
	for _, item := range []struct{ column, value string }{
		{"host", f.Host}, {"project", f.Project},
		{"conversation", f.Conversation}, {"source", f.Source},
	} {
		if item.value != "" {
			conditions = append(conditions, prefix+item.column+" = ?")
			arguments = append(arguments, item.value)
		}
	}
	if !f.Since.IsZero() {
		conditions = append(conditions, prefix+"updated_at >= ?")
		arguments = append(arguments, f.Since.UTC().Format(time.RFC3339))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(conditions, " AND "), arguments
}

// schema 是建表语句（幂等）。messages_fts 是**无内容**（contentless）的 FTS5
// 虚表：只建倒排索引，文本本体留在 messages 表。这样文本只存一份，
// session_read 从 messages 有序分页读，session_search 走 messages_fts 的
// 倒排索引再按 rowid 回表。
//
// 分词器用 trigram 而不是默认的 unicode61：会话记录以中文为主而中文没有词
// 边界，unicode61 会把整句当成一个 token，检索等于失效。trigram 按三元组建
// 索引，配合默认的 case_sensitive=0（LIKE 语义），与旧实现
// strings.Contains(strings.ToLower(..)) 的子串语义一致。
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	host               TEXT NOT NULL,
	project            TEXT NOT NULL,
	session            TEXT NOT NULL,
	source             TEXT NOT NULL DEFAULT '',
	conversation       TEXT NOT NULL DEFAULT '',
	transport          TEXT NOT NULL DEFAULT '',
	title              TEXT NOT NULL DEFAULT '',
	model              TEXT NOT NULL DEFAULT '',
	lines              INTEGER NOT NULL DEFAULT 0,
	bytes              INTEGER NOT NULL DEFAULT 0,
	created_at         TEXT NOT NULL DEFAULT '',
	updated_at         TEXT NOT NULL DEFAULT '',
	archived_at        TEXT NOT NULL DEFAULT '',
	size               INTEGER NOT NULL DEFAULT 0,
	mod_time           TEXT NOT NULL DEFAULT '',
	first_user_text    TEXT NOT NULL DEFAULT '',
	last_assistant_text TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (host, project, session)
);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at);
CREATE INDEX IF NOT EXISTS idx_sessions_conversation ON sessions(conversation);

CREATE TABLE IF NOT EXISTS messages (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	host      TEXT NOT NULL,
	project   TEXT NOT NULL,
	session   TEXT NOT NULL,
	line      INTEGER NOT NULL,
	role      TEXT NOT NULL,
	text      TEXT NOT NULL,
	timestamp TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_messages_key ON messages(host, project, session, line);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	text, content='', tokenize='trigram'
);
`

// Store 是会话记录索引的句柄。零值不可用，须 Open。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）索引并启用 WAL。path 为空返回 nil（未配置存储即
// 未启用索引，调用方以 nil Store 安全跳过）。
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建会话索引目录 %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("打开会话索引 %s: %w", path, err)
	}
	// SQLite 单写者：串行化所有写连接，避免 SQLITE_BUSY
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化会话索引 %s: %w", path, err)
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

// Watermark 返回已归档记录的增量水位。ok 为 false 表示这条会话还没归档过。
func (s *Store) Watermark(key Key) (size int64, modTime string, ok bool, err error) {
	if s == nil {
		return 0, "", false, nil
	}
	if !key.Valid() {
		return 0, "", false, fmt.Errorf("非法的会话键：%q/%q/%q", key.Host, key.Project, key.Session)
	}
	err = s.db.QueryRow(
		`SELECT size, mod_time FROM sessions WHERE host = ? AND project = ? AND session = ?`,
		key.Host, key.Project, key.Session).Scan(&size, &modTime)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("读取会话水位 %s: %w", key, err)
	}
	return size, modTime, true, nil
}

// Upsert 写入一条记录及其消息：同一会话整体替换（会话是只增的，但重传时
// 仍以最后一次为准，避免留下被截断的旧消息）。
func (s *Store) Upsert(record Record, messages []Message) error {
	if s == nil {
		return nil
	}
	if !record.Key.Valid() {
		return fmt.Errorf("非法的会话键：%q/%q/%q", record.Host, record.Project, record.Session)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceMessages(tx, record.Key, messages); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO sessions (host, project, session, source, conversation, transport,
			title, model, lines, bytes, created_at, updated_at, archived_at,
			size, mod_time, first_user_text, last_assistant_text)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host, project, session) DO UPDATE SET
			source=excluded.source, conversation=excluded.conversation,
			transport=excluded.transport, title=excluded.title, model=excluded.model,
			lines=excluded.lines, bytes=excluded.bytes, updated_at=excluded.updated_at,
			archived_at=excluded.archived_at, size=excluded.size, mod_time=excluded.mod_time,
			first_user_text=excluded.first_user_text, last_assistant_text=excluded.last_assistant_text`,
		record.Host, record.Project, record.Session, record.Source, record.Conversation,
		record.Transport, record.Title, record.Model, record.Lines, record.Bytes,
		record.CreatedAt, record.UpdatedAt, record.ArchivedAt,
		record.Size, record.ModTime, record.FirstUserText, record.LastAssistantText); err != nil {
		return fmt.Errorf("写入会话元数据 %s: %w", record.Key, err)
	}
	return tx.Commit()
}

// replaceMessages 用新消息整体替换某会话的消息，并同步 FTS 索引。
//
// 无内容（contentless）FTS 表不会随基表的 DELETE 自动更新，必须逐条发
// 'delete' 命令——这正是它容易悄悄失同步的地方，所以删除前先把旧文本读出来。
func replaceMessages(tx *sql.Tx, key Key, messages []Message) error {
	rows, err := tx.Query(
		`SELECT id, text FROM messages WHERE host = ? AND project = ? AND session = ?`,
		key.Host, key.Project, key.Session)
	if err != nil {
		return fmt.Errorf("读取旧消息 %s: %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO messages_fts(messages_fts, rowid, text) VALUES('delete', ?, ?)`,
			id, text); err != nil {
			return fmt.Errorf("清理会话全文索引 %s: %w", key, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM messages WHERE host = ? AND project = ? AND session = ?`,
		key.Host, key.Project, key.Session); err != nil {
		return fmt.Errorf("删除旧消息 %s: %w", key, err)
	}
	for _, message := range messages {
		result, err := tx.Exec(
			`INSERT INTO messages (host, project, session, line, role, text, timestamp)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			key.Host, key.Project, key.Session, message.Line, message.Role,
			message.Text, message.Timestamp)
		if err != nil {
			return fmt.Errorf("写入消息 %s 第 %d 行: %w", key, message.Line, err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO messages_fts(rowid, text) VALUES (?, ?)`, id, message.Text); err != nil {
			return fmt.Errorf("写入全文索引 %s 第 %d 行: %w", key, message.Line, err)
		}
	}
	return nil
}

// List 返回符合条件的会话元数据（按更新时间倒序，同刻按键升序）。
func (s *Store) List(filter Filter, limit int) ([]Record, error) {
	if s == nil {
		return nil, nil
	}
	condition, arguments := filter.match("")
	query := `SELECT host, project, session, source, conversation, transport, title, model,
	                 lines, bytes, created_at, updated_at, archived_at, size, mod_time,
	                 first_user_text, last_assistant_text
	          FROM sessions WHERE 1=1` + condition + ` ORDER BY updated_at DESC, host, project, session`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("列出会话：%w", err)
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(records, compareRecord)
	return records, nil
}

// compareRecord 是列表的排序：更新时间倒序，同刻按键升序让结果稳定可测。
func compareRecord(a, b Record) int {
	if order := cmp.Compare(b.UpdatedAt, a.UpdatedAt); order != 0 {
		return order
	}
	return cmp.Compare(a.Key.String(), b.Key.String())
}

// Read 读取一条会话的消息（offset/limit 按消息计；limit<=0 表示不限制）。
func (s *Store) Read(key Key, offset, limit int) ([]Message, error) {
	if s == nil {
		return nil, nil
	}
	if !key.Valid() {
		return nil, fmt.Errorf("非法的会话键：%q/%q/%q", key.Host, key.Project, key.Session)
	}
	query := `SELECT line, role, text, timestamp FROM messages
	          WHERE host = ? AND project = ? AND session = ? ORDER BY line`
	arguments := []any{key.Host, key.Project, key.Session}
	// LIMIT/OFFSET 必须写成一条子句：SQLite 不接受 `LIMIT -1 OFFSET ? LIMIT ?`。
	// limit<=0 表示不限量，此时若有 offset 用 `LIMIT -1` 占位（SQLite 的惯用写法）。
	switch {
	case limit > 0 && offset > 0:
		query += " LIMIT ? OFFSET ?"
		arguments = append(arguments, limit, offset)
	case limit > 0:
		query += " LIMIT ?"
		arguments = append(arguments, limit)
	case offset > 0:
		query += " LIMIT -1 OFFSET ?"
		arguments = append(arguments, offset)
	}
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("读取会话消息 %s: %w", key, err)
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.Line, &message.Role, &message.Text, &message.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// Search 在消息正文里做子串检索（大小写不敏感），返回命中消息及其所属会话。
//
// 长度 <3 的检索词退回 LIKE 全表扫：trigram 分词器需要至少三个字符才建得出
// 索引，直接 MATCH 会静默返回空。索引在本机上，一次全表扫是毫秒级，用它换
// 与旧实现完全一致的短词语义比让检索"悄悄查不到"划算。
func (s *Store) Search(query string, filter Filter, limit int) ([]Hit, error) {
	if s == nil {
		return nil, nil
	}
	needle := strings.TrimSpace(query)
	if needle == "" {
		return nil, fmt.Errorf("检索词为空")
	}
	if limit <= 0 {
		limit = 50
	}
	condition, arguments := filter.match("s.")
	var rows *sql.Rows
	var err error
	if len([]rune(needle)) < 3 {
		rows, err = s.db.Query(`SELECT m.line, m.role, m.text, m.timestamp,
		                              s.host, s.project, s.session,
		                              s.conversation, s.source, s.transport
		                       FROM messages m JOIN sessions s
		                         ON s.host = m.host AND s.project = m.project AND s.session = m.session
		                       WHERE m.text LIKE ? ESCAPE '\'`+condition+
			` ORDER BY s.updated_at DESC, m.line LIMIT ?`,
			append([]any{"%" + escapeLike(needle) + "%"}, append(arguments, limit)...)...)
	} else {
		rows, err = s.db.Query(`SELECT m.line, m.role, m.text, m.timestamp,
		                              s.host, s.project, s.session,
		                              s.conversation, s.source, s.transport
		                       FROM messages_fts f JOIN messages m ON m.id = f.rowid
		                       JOIN sessions s
		                         ON s.host = m.host AND s.project = m.project AND s.session = m.session
		                       WHERE messages_fts MATCH ?`+condition+`
		                       ORDER BY s.updated_at DESC, m.line LIMIT ?`,
			append([]any{ftsPhrase(needle)}, append(arguments, limit)...)...)
	}
	if err != nil {
		return nil, fmt.Errorf("检索会话记录：%w", err)
	}
	defer rows.Close()
	hits := []Hit{}
	for rows.Next() {
		var hit Hit
		if err := rows.Scan(&hit.Line, &hit.Role, &hit.Text, &hit.Timestamp,
			&hit.Host, &hit.Project, &hit.Session,
			&hit.Conversation, &hit.Source, &hit.Transport); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// ftsPhrase 把用户输入包成 FTS5 的字符串常量。
//
// 必须包引号：检索词里出现的 "-" "*" ":" 等是 FTS5 的语法符号，直接 MATCH
// 会报语法错或改变语义。双写内层引号是 FTS5 的转义方式。
func ftsPhrase(text string) string {
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}

// escapeLike 转义 LIKE 模式里的特殊字符，让检索词按字面匹配。
func escapeLike(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
}

// Conversations 返回索引里出现过的会话实体（去重）及各自的记录条数。
func (s *Store) Conversations() (map[string]int, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT conversation, COUNT(*) FROM sessions WHERE conversation <> '' GROUP BY conversation`)
	if err != nil {
		return nil, fmt.Errorf("列出会话实体：%w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var conversation string
		var count int
		if err := rows.Scan(&conversation, &count); err != nil {
			return nil, err
		}
		counts[conversation] = count
	}
	return counts, rows.Err()
}

// scanner 是 scanRecord 依赖的最小行扫描面（*sql.Rows 与 *sql.Row 都满足）。
type scanner interface {
	Scan(dest ...any) error
}

// scanRecord 从一行结果读出一条记录。列顺序必须与 List 的 SELECT 一致。
func scanRecord(row scanner) (Record, error) {
	var record Record
	err := row.Scan(&record.Host, &record.Project, &record.Session,
		&record.Source, &record.Conversation, &record.Transport,
		&record.Title, &record.Model, &record.Lines, &record.Bytes,
		&record.CreatedAt, &record.UpdatedAt, &record.ArchivedAt,
		&record.Size, &record.ModTime,
		&record.FirstUserText, &record.LastAssistantText)
	if err != nil {
		return Record{}, err
	}
	return record, nil
}
