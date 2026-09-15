package sessionstore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ChatSessionFile 是每个微信/通道会话工作目录里的元数据文件名（由对话桥写入）。
const ChatSessionFile = "session.json"

// chatInfo 是从对话会话工作目录的 session.json 里读到的映射信息。
type chatInfo struct {
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	Title          string `json:"title"`
	Model          string `json:"model"`
	Transport      string `json:"transport"`
	Workdir        string `json:"workdir"`
}

// CollectOptions 描述一次采集：从本地文本记录目录扫出会话，按需增量。
type CollectOptions struct {
	// Root 是 claude 配置根（<配置目录>/claude），记录在 <Root>/projects/<项目>/<会话>.jsonl
	Root string
	// Host 是宿主机标签（默认取 os.Hostname），用于区分不同机器上同名项目
	Host string
	// ChatDir 是对话会话状态目录（<配置目录>/chat）：读各会话的 session.json 取
	// conversation/transport/title 映射
	ChatDir string
	// All 为真时忽略增量状态，全部重新采集
	All bool
	// StatePath 是增量状态文件（记录已推送的大小与修改时间）
	StatePath string
}

// Collect 扫描本地记录并返回需要推送的会话（增量：大小与修改时间都没变则跳过）。
func Collect(options CollectOptions) (Batch, error) {
	root := strings.TrimSpace(options.Root)
	if root == "" {
		return Batch{}, fmt.Errorf("缺少记录根目录")
	}
	host := strings.TrimSpace(options.Host)
	if host == "" {
		if name, err := os.Hostname(); err == nil {
			host = name
		} else {
			host = "unknown"
		}
	}
	chatSessions := readChatSessions(options.ChatDir)
	state := State{}
	if !options.All {
		state = LoadState(options.StatePath)
	}

	batch := Batch{Sessions: make([]Session, 0, 8)}
	projectsDir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return batch, nil
		}
		return batch, err
	}
	for _, projectEntry := range entries {
		if !projectEntry.IsDir() {
			continue
		}
		project := projectEntry.Name()
		files, err := os.ReadDir(filepath.Join(projectsDir, project))
		if err != nil {
			continue
		}
		for _, fileEntry := range files {
			name := fileEntry.Name()
			if fileEntry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			session := Session{Meta: Meta{
				Key:    Key{Host: host, Project: project, Session: strings.TrimSuffix(name, ".jsonl")},
				Source: DetectSource(project),
			}}
			path := filepath.Join(projectsDir, project, name)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if entry, ok := state[session.Key.String()]; ok && !options.All {
				if entry.Size == info.Size() && entry.ModTime == info.ModTime().UTC().Format(time.RFC3339Nano) {
					continue
				}
			}
			lines, err := readLines(path)
			if err != nil {
				continue
			}
			session.Lines = lines
			if chat, ok := chatSessions[session.Session]; ok {
				session.Source = "chat"
				session.Conversation = chat.ConversationID
				session.Transport = chat.Transport
				session.Title = chat.Title
				session.Model = chat.Model
			}
			session.CreatedAt = info.ModTime().UTC().Format(time.RFC3339)
			session.UpdatedAt = session.CreatedAt
			session.FirstUserText, session.LastAssistantText = Summarize(lines)
			batch.Sessions = append(batch.Sessions, session)
		}
	}
	return batch, nil
}

// SessionFor 定位并读取**单个** claude 会话的记录（按会话 id 在 projects/* 下找），
// 供对话桥每轮结束后即时归档；找不到返回 ok=false。
func SessionFor(options CollectOptions, sessionID string) (Session, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Session{}, false, nil
	}
	host := strings.TrimSpace(options.Host)
	if host == "" {
		if name, err := os.Hostname(); err == nil {
			host = name
		} else {
			host = "unknown"
		}
	}
	projectsDir := filepath.Join(strings.TrimSpace(options.Root), "projects")
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, false, nil
		}
		return Session{}, false, err
	}
	for _, projectEntry := range projects {
		if !projectEntry.IsDir() {
			continue
		}
		path := filepath.Join(projectsDir, projectEntry.Name(), sessionID+".jsonl")
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		lines, err := readLines(path)
		if err != nil {
			return Session{}, false, err
		}
		session := Session{
			Meta: Meta{
				Key:       Key{Host: host, Project: projectEntry.Name(), Session: sessionID},
				Source:    DetectSource(projectEntry.Name()),
				CreatedAt: info.ModTime().UTC().Format(time.RFC3339),
				UpdatedAt: info.ModTime().UTC().Format(time.RFC3339),
			},
			Lines: lines,
		}
		if chat, ok := readChatSessions(options.ChatDir)[sessionID]; ok {
			session.Source = "chat"
			session.Conversation = chat.ConversationID
			session.Transport = chat.Transport
			session.Title = chat.Title
			session.Model = chat.Model
		}
		session.FirstUserText, session.LastAssistantText = Summarize(lines)
		return session, true, nil
	}
	return Session{}, false, nil
}

// Push 采集并推送：逐条上传，成功一条记一条增量状态（失败不记，下次重试）。
func Push(ctx context.Context, options CollectOptions, client *Client, logf func(string, ...any)) (PushResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	batch, err := Collect(options)
	if err != nil {
		return PushResult{}, err
	}
	state := LoadState(options.StatePath)
	result := PushResult{}
	for _, session := range batch.Sessions {
		singleStart := time.Now()
		if _, err := client.Push(ctx, Batch{Sessions: []Session{session}}); err != nil {
			return result, fmt.Errorf("推送 %s: %w", session.Key.String(), err)
		}
		result.Stored++
		path := filepath.Join(options.Root, "projects", session.Project, session.Session+".jsonl")
		if info, err := os.Stat(path); err == nil {
			state[session.Key.String()] = StateEntry{
				Size:     info.Size(),
				ModTime:  info.ModTime().UTC().Format(time.RFC3339Nano),
				PushedAt: singleStart.UTC().Format(time.RFC3339),
			}
		}
		logf("已推送 %s（%d 行，来源 %s）", session.Key.String(), len(session.Lines), session.Source)
	}
	if options.StatePath != "" && result.Stored > 0 {
		if err := state.Save(options.StatePath); err != nil {
			return result, err
		}
	}
	return result, nil
}

// State 是增量推送状态：键 → 已推送文件的大小与修改时间。
type State map[string]StateEntry

// StateEntry 是一条已推送记录的指纹。
type StateEntry struct {
	Size     int64  `json:"size"`
	ModTime  string `json:"mod_time"`
	PushedAt string `json:"pushed_at,omitempty"`
}

// LoadState 读取增量状态（文件缺失/损坏返回空状态，最多重新推送一次）。
func LoadState(path string) State {
	state := State{}
	if strings.TrimSpace(path) == "" {
		return state
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}
	}
	return state
}

// Save 原子写入增量状态（0600）。
func (s State) Save(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// readChatSessions 扫描对话会话工作目录里的 session.json，按 claude 会话 id 建索引
// ——记录文件就是 <session-id>.jsonl，用它做关联不需要推导 claude 的项目名编码。
func readChatSessions(chatDir string) map[string]chatInfo {
	sessions := map[string]chatInfo{}
	chatDir = strings.TrimSpace(chatDir)
	if chatDir == "" {
		return sessions
	}
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		return sessions
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(chatDir, entry.Name(), ChatSessionFile)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info chatInfo
		if err := json.Unmarshal(data, &info); err != nil || strings.TrimSpace(info.SessionID) == "" {
			continue
		}
		if info.Transport == "" {
			info.Transport = "weixin"
		}
		sessions[info.SessionID] = info
	}
	return sessions
}

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	lines := make([]string, 0, 256)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}
