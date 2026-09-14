// Package instances 管理多实例配置文件（config.json）。
//
// 一个 instance 对应一台 Gitea 站点；每个 instance 登记仓库清单与三类凭据：
// admin（高权限，读分支保护 + setup 操作用）、reviewer（内容评审机器人）、
// merger（状态评审/合并机器人）。setup 命令负责创建后两类账号与令牌，并把
// 结果回写本文件；其余命令按 instance × repo 迭代执行。
package instances

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// 默认机器人账号名：reviewer 是内容评审者，merger 是状态评审者（会签/合并）。
const (
	DefaultReviewerName = "ai"
	DefaultMergerName   = "merge"
)

// File 是 config.json 的根。
type File struct {
	Instances []Instance `json:"instances"`
}

// Instance 是一台 Gitea 站点及其仓库与凭据。
type Instance struct {
	Host string `json:"host"`
	// AdminToken 是高权限令牌：读取分支保护需要 repo admin；setup 也用它建
	// 账号与令牌。留空时运行期分支保护读取自动回退严格模式。
	AdminToken string `json:"admin_token,omitempty"`
	// AdminOAuth 是 OAuth 登录留下的刷新凭据：access token 短期有效不落盘，
	// refresh token 长期有效，运行期按需换取（用 OAuth 初始化时写入）。
	AdminOAuth *OAuthCredential `json:"admin_oauth,omitempty"`
	Reviewer   Account          `json:"reviewer,omitempty"`
	Merger     Account          `json:"merger,omitempty"`
	Repos      []Repo           `json:"repos"`
}

// OAuthCredential 是 OAuth2 刷新凭据。
type OAuthCredential struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	RefreshToken string `json:"refresh_token"`
}

// Account 是一个机器人账号及其访问令牌。
type Account struct {
	Name  string `json:"name"`
	Token string `json:"token,omitempty"`
}

// Repo 是一个被管理的仓库。JSON 里可写 "owner/name" 简写，或 {"name":...,
// "dir":...} 对象：dir 是该仓库的本地检出路径，调度引擎的 review/triage
// 会话需要它（单仓库可省略，退回启动目录的检出）。
type Repo struct {
	Name string `json:"name"`
	Dir  string `json:"dir,omitempty"`
	// MergerToken 是该仓库专属的 merger 令牌（仓库级 Actions workflow 会签/
	// 合并用）。每个项目独立令牌，互不影响；由 setup 生成并写入仓库 secret。
	MergerToken string `json:"merger_token,omitempty"`
}

func (r *Repo) UnmarshalJSON(data []byte) error {
	var shorthand string
	if err := json.Unmarshal(data, &shorthand); err == nil {
		r.Name = shorthand
		r.Dir = ""
		return nil
	}
	type plain Repo
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf(`仓库条目必须是 "owner/name" 或 {"name": "owner/name", "dir": "..."}`)
	}
	*r = Repo(value)
	return nil
}

// MarshalJSON 无 dir 时写回字符串简写，保持配置文件简洁。
func (r Repo) MarshalJSON() ([]byte, error) {
	if r.Dir == "" && r.MergerToken == "" {
		return json.Marshal(r.Name)
	}
	type plain Repo
	return json.Marshal(plain(r))
}

// RepoNames 返回非空仓库名清单。
func (i Instance) RepoNames() []string {
	names := make([]string, 0, len(i.Repos))
	for _, repo := range i.Repos {
		if repo.Name != "" {
			names = append(names, repo.Name)
		}
	}
	return names
}

// FindRepo 按 owner/name 查找仓库配置。
func (i Instance) FindRepo(name string) (Repo, bool) {
	for _, repo := range i.Repos {
		if repo.Name == name {
			return repo, true
		}
	}
	return Repo{}, false
}

// Load 读取并校验配置文件。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 无效: %w", path, err)
	}
	return &file, nil
}

// Save 原子写入配置文件（0600）；先写同目录临时文件再改名，避免半截文件。
func Save(path string, file *File) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".config-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// Normalize 填充默认值并清理空白，幂等。
func (f *File) Normalize() {
	for index := range f.Instances {
		f.Instances[index].Normalize()
	}
}

// Normalize 填充 instance 内的默认值并清理空白，幂等。
func (i *Instance) Normalize() {
	i.Host = strings.TrimRight(strings.TrimSpace(i.Host), "/")
	if i.Reviewer.Name == "" {
		i.Reviewer.Name = DefaultReviewerName
	}
	if i.Merger.Name == "" {
		i.Merger.Name = DefaultMergerName
	}
	for repoIndex := range i.Repos {
		i.Repos[repoIndex].Name = strings.TrimSpace(i.Repos[repoIndex].Name)
		i.Repos[repoIndex].Dir = strings.TrimSpace(i.Repos[repoIndex].Dir)
	}
}

// Validate 校验 host、账号与仓库。必须在 Normalize 之后调用。
func (f *File) Validate() error {
	if len(f.Instances) == 0 {
		return fmt.Errorf("instances 不能为空")
	}
	seen := make(map[string]int, len(f.Instances))
	for index := range f.Instances {
		if err := f.Instances[index].Validate(); err != nil {
			return fmt.Errorf("instances[%d]: %w", index, err)
		}
		if previous, ok := seen[f.Instances[index].Host]; ok {
			return fmt.Errorf("instances[%d] 与 instances[%d] 的 host 重复：%s", index, previous, f.Instances[index].Host)
		}
		seen[f.Instances[index].Host] = index
	}
	return nil
}

// Validate 校验单个 instance。必须在 Normalize 之后调用。
func (i Instance) Validate() error {
	parsed, err := url.Parse(i.Host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("host 必须是绝对 HTTP(S) URL：%q", i.Host)
	}
	if i.Reviewer.Name == i.Merger.Name {
		return fmt.Errorf("reviewer 与 merger 不能是同一账号（%s）", i.Reviewer.Name)
	}
	if i.AdminOAuth != nil {
		if i.AdminOAuth.ClientID == "" || i.AdminOAuth.RefreshToken == "" {
			return fmt.Errorf("admin_oauth 需要 client_id 与 refresh_token")
		}
	}
	for _, repo := range i.Repos {
		if _, _, err := ParseRepoName(repo.Name); err != nil {
			return err
		}
	}
	return nil
}

// ParseRepoName 解析 owner/name。
func ParseRepoName(name string) (owner, repository string, err error) {
	owner, repository, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", "", fmt.Errorf("仓库必须使用 owner/name 格式：%q", name)
	}
	return owner, repository, nil
}

// 平台标准配置目录下的命名空间/应用名（Linux: $XDG_CONFIG_HOME 或
// ~/.config；macOS: ~/Library/Application Support；Windows: %AppData%）。
const (
	configNamespace = "Cosmic-Developers-Union"
	configApp       = "assistant"
)

// DefaultConfigDir 返回平台标准配置目录。
func DefaultConfigDir() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, configNamespace, configApp), nil
}

// DefaultConfigPath 是标准配置目录下的 config.json 路径。
func DefaultConfigPath() (string, error) {
	directory, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "config.json"), nil
}

// DefaultRepoDir 返回受管克隆（基线检出）的默认落点：
// <数据目录>/Cosmic-Developers-Union/assistant/repos/<host>/<owner>/<name>；
// 未配置 repo.dir 的仓库在这里自动 clone 并保持与 origin/<base> 一致，run 与
// 当前目录彻底解耦。数据目录取 XDG_DATA_HOME，缺省 ~/.local/share。
func DefaultRepoDir(host, fullName string) (string, error) {
	root, slug, owner, name, err := managedPathParts(host, fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "repos", slug, owner, name), nil
}

// DefaultRepoStateDir 返回受管仓库的状态目录（日志、单飞锁）：
// <数据目录>/Cosmic-Developers-Union/assistant/state/<host>/<owner>/<name>；
// 放在检出之外，避免被 SyncMirror 的 clean -fd 波及。
func DefaultRepoStateDir(host, fullName string) (string, error) {
	root, slug, owner, name, err := managedPathParts(host, fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "state", slug, owner, name), nil
}

func managedPathParts(host, fullName string) (root, slug, owner, name string, err error) {
	home := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if home == "" {
		userHome, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", "", "", fmt.Errorf("定位用户目录: %w", homeErr)
		}
		home = filepath.Join(userHome, ".local", "share")
	}
	owner, name, err = ParseRepoName(fullName)
	if err != nil {
		return "", "", "", "", err
	}
	slug, err = HostSlug(host)
	if err != nil {
		return "", "", "", "", err
	}
	root = filepath.Join(home, configNamespace, configApp)
	return root, slug, owner, name, nil
}

// HostSlug 把站点地址折成目录/路径名：去 scheme，保留 host[:port]，其余字符
// 替换为 -（受管目录与 worktree 根共用）。
func HostSlug(host string) (string, error) {
	trimmed := strings.TrimSpace(host)
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("无法解析平台地址：%q", host)
	}
	var builder strings.Builder
	for _, character := range parsed.Host {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.', character == '-':
			builder.WriteRune(character)
		default:
			builder.WriteRune('-')
		}
	}
	return builder.String(), nil
}
