// Package conversations 是「会话实体」的映射层：一条会话不由通道唯一决定——微信
// 用户、将来的其它通道（Telegram/Slack/HTTP）、甚至手工登记的会话都可以映射到同一个
// conversation；claude 会话（/new 后是一个新的 session id）挂在 conversation 下。
//
// 落点：<配置目录>/chat/conversations.json（0600）。会话记录按 conversation 归类，
// 跨通道可查（见 internal/sessionstore）。
package conversations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CurrentVersion 是文件格式版本。v2 起会话实体可携带用户 /agent 切换的 agent 名
// （可选字段：v1 文件读入不报错，旧二进制读 v2 文件忽略该字段）。
const CurrentVersion = 2

// Binding 是一个通道绑定：哪个通道的哪个用户属于这个会话。
type Binding struct {
	Transport string `json:"transport"`
	User      string `json:"user"`
}

// Conversation 是一条会话实体。
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at,omitempty"`
	Bindings  []Binding `json:"bindings,omitempty"`
	// Sessions 是该会话下的 claude 会话 id（`/new` 后追加，最新在后）
	Sessions []string `json:"sessions,omitempty"`
	// Agent 是用户用 /agent 命令为本会话选定的 agent 名；空表示跟随通道/全局默认
	Agent string `json:"agent,omitempty"`
}

// File 是 conversations.json 的根。
type File struct {
	Version       int            `json:"version"`
	Conversations []Conversation `json:"conversations,omitempty"`
}

// Load 读取映射文件；文件不存在返回空表（不是错误）。
func Load(path string) (*File, error) {
	file := &File{Version: CurrentVersion}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return file, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, file); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if file.Version == 0 {
		file.Version = CurrentVersion
	}
	return file, nil
}

// Save 原子写入（0600）。
func Save(path string, file *File) error {
	file.Version = CurrentVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Find 按通道绑定反查会话。
func (f *File) Find(transport, user string) (Conversation, bool) {
	for _, conversation := range f.Conversations {
		for _, binding := range conversation.Bindings {
			if binding.Transport == transport && binding.User == user {
				return conversation, true
			}
		}
	}
	return Conversation{}, false
}

// ByID 按 id 取会话。
func (f *File) ByID(id string) (Conversation, bool) {
	for _, conversation := range f.Conversations {
		if conversation.ID == id {
			return conversation, true
		}
	}
	return Conversation{}, false
}

// Ensure 取（必要时创建）某通道用户对应的会话；created 表示本次新建。
// 会话 id 由首个绑定确定性派生（同一通道用户重复调用得到同一个 id）。
func (f *File) Ensure(transport, user, title string) (Conversation, bool) {
	transport = strings.TrimSpace(transport)
	user = strings.TrimSpace(user)
	if transport == "" || user == "" {
		return Conversation{}, false
	}
	if existing, ok := f.Find(transport, user); ok {
		return existing, false
	}
	now := time.Now().UTC().Format(time.RFC3339)
	conversation := Conversation{
		ID:        deriveID(transport, user),
		Title:     strings.TrimSpace(title),
		CreatedAt: now,
		UpdatedAt: now,
		Bindings:  []Binding{{Transport: transport, User: user}},
	}
	// 极端情况下（不同通道的派生 id 撞车）退化为加后缀
	for _, existing := range f.Conversations {
		if existing.ID == conversation.ID {
			conversation.ID += "-" + deriveID(user, transport)[3:7]
			break
		}
	}
	f.Conversations = append(f.Conversations, conversation)
	return conversation, true
}

// Bind 把一个通道绑定加到既有会话上（例如把另一个通道的同一人并入同一会话）。
func (f *File) Bind(id, transport, user string) error {
	transport, user = strings.TrimSpace(transport), strings.TrimSpace(user)
	if transport == "" || user == "" {
		return fmt.Errorf("通道与用户都不能为空")
	}
	if existing, ok := f.Find(transport, user); ok && existing.ID != id {
		return fmt.Errorf("%s/%s 已绑定到会话 %s", transport, user, existing.ID)
	}
	for index := range f.Conversations {
		if f.Conversations[index].ID != id {
			continue
		}
		for _, binding := range f.Conversations[index].Bindings {
			if binding.Transport == transport && binding.User == user {
				return nil
			}
		}
		f.Conversations[index].Bindings = append(f.Conversations[index].Bindings, Binding{Transport: transport, User: user})
		f.Conversations[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	}
	return fmt.Errorf("会话 %s 不存在", id)
}

// SetAgent 记录用户为本会话选定的 agent（/agent 命令）；空名表示清除选择、
// 回到通道/全局默认。
func (f *File) SetAgent(id, name string) error {
	name = strings.TrimSpace(name)
	for index := range f.Conversations {
		if f.Conversations[index].ID != id {
			continue
		}
		f.Conversations[index].Agent = name
		f.Conversations[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	}
	return fmt.Errorf("会话 %s 不存在", id)
}

// AgentOf 返回会话上记录的 agent 名（不存在或未选择时为空串）。
func (f *File) AgentOf(id string) string {
	conversation, ok := f.ByID(id)
	if !ok {
		return ""
	}
	return conversation.Agent
}

// Attach 把 claude 会话 id 挂到该 conversation 下（去重，最新在后）。
func (f *File) Attach(id, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("会话 id 不能为空")
	}
	for index := range f.Conversations {
		if f.Conversations[index].ID != id {
			continue
		}
		for _, existing := range f.Conversations[index].Sessions {
			if existing == sessionID {
				return nil
			}
		}
		f.Conversations[index].Sessions = append(f.Conversations[index].Sessions, sessionID)
		f.Conversations[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	}
	return fmt.Errorf("会话 %s 不存在", id)
}

// Bindings 返回某会话的全部通道绑定（诊断/展示用）。
func (c Conversation) BindingLabels() []string {
	labels := make([]string, 0, len(c.Bindings))
	for _, binding := range c.Bindings {
		labels = append(labels, binding.Transport+":"+binding.User)
	}
	sort.Strings(labels)
	return labels
}

// deriveID 由首个绑定派生稳定的会话 id（不透明，避免泄漏通道标识）。
func deriveID(transport, user string) string {
	sum := sha256.Sum256([]byte(transport + "\x00" + user))
	return "c-" + hex.EncodeToString(sum[:4])
}
