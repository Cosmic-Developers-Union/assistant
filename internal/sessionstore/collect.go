package sessionstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
)

// ChatSessionFile 是每个聊天会话工作目录里的元数据文件名（由对话桥写入）。
const ChatSessionFile = "session.json"

// chatInfo 是从会话工作目录的 session.json 里读到的映射信息。
type chatInfo struct {
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	Title          string `json:"title"`
	Model          string `json:"model"`
	Transport      string `json:"transport"`
	Workdir        string `json:"workdir"`
}

// CollectOptions 描述一次归档采集：从哪里扫本地记录、挂什么宿主机标签。
type CollectOptions struct {
	// Root 是 claude 配置根（<配置目录>/claude），记录在 <Root>/projects/<项目>/<会话>.jsonl
	Root string
	// Host 是宿主机标签（默认取 os.Hostname），用于区分不同机器上同名项目
	Host string
	// ChatDir 是聊天会话状态目录（<配置目录>/chat）：读各会话的 session.json 取
	// conversation/transport/title 映射
	ChatDir string
}

// Candidate 是一条待归档的本地会话文件。
//
// 扫描只做 os.Stat 不读内容：会话记录动辄数 MB，而绝大多数文件自上次归档后没变过，
// 判断"变没变"只需大小与修改时间，水位存在索引里（见 sessionindex.Watermark）。
type Candidate struct {
	Key     sessionindex.Key
	Path    string
	Size    int64
	ModTime string
}

// Scan 扫描本地记录目录，返回全部候选（不做增量判断——水位在索引里）。
//
// 记录目录不存在不算错误（还没跑过 claude 的机器上就是没有），返回空切片。
func Scan(options CollectOptions) ([]Candidate, error) {
	root := strings.TrimSpace(options.Root)
	if root == "" {
		return nil, fmt.Errorf("缺少记录根目录")
	}
	host := resolveHost(options.Host)
	projectsDir := filepath.Join(root, "projects")
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取记录目录 %s: %w", projectsDir, err)
	}
	candidates := make([]Candidate, 0, 16)
	for _, projectEntry := range projects {
		if !projectEntry.IsDir() {
			continue
		}
		project := projectEntry.Name()
		files, err := os.ReadDir(filepath.Join(projectsDir, project))
		if err != nil {
			continue // 单个项目读不到不影响其余项目
		}
		for _, fileEntry := range files {
			name := fileEntry.Name()
			if fileEntry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			path := filepath.Join(projectsDir, project, name)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			candidates = append(candidates, Candidate{
				Key:     sessionindex.Key{Host: host, Project: project, Session: strings.TrimSuffix(name, ".jsonl")},
				Path:    path,
				Size:    info.Size(),
				ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
			})
		}
	}
	return candidates, nil
}

// Find 按 claude 会话 id 定位单个候选（在 projects/* 下找 <id>.jsonl）。
//
// 供对话桥每轮结束后即时归档：只知道会话 id，不知道它落在哪个项目目录，所以这里
// 允许跨项目查找；找不到返回 ok=false（会话可能还没落盘）。
func Find(options CollectOptions, sessionID string) (Candidate, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Candidate{}, false, nil
	}
	host := resolveHost(options.Host)
	projectsDir := filepath.Join(strings.TrimSpace(options.Root), "projects")
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return Candidate{}, false, nil
		}
		return Candidate{}, false, fmt.Errorf("读取记录目录 %s: %w", projectsDir, err)
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
		return Candidate{
			Key:     sessionindex.Key{Host: host, Project: projectEntry.Name(), Session: sessionID},
			Path:    path,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
		}, true, nil
	}
	return Candidate{}, false, nil
}

// resolveHost 解析宿主机标签：显式给的优先，否则主机名，再不行用 unknown。
func resolveHost(host string) string {
	if trimmed := strings.TrimSpace(host); trimmed != "" {
		return trimmed
	}
	if name, err := os.Hostname(); err == nil && strings.TrimSpace(name) != "" {
		return name
	}
	return "unknown"
}

// readChatSessions 扫描聊天会话工作目录里的 session.json，按 claude 会话 id 建索引
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

// readLines 读出一个记录文件的全部原始行。
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
