// Package credentials 管理 credentials.json：本地凭据库，按 (host, user, purpose)
// 索引「身份」与「用途令牌」。
//
// 身份（Identity）是一次 username/password 登录确定的事实：这台站点当前以谁登录、
// 该账号是不是实例管理员。令牌（Credential）按用途派生，彼此不借用：
//
//	mcp    开发者本地工具面（编辑器/CLI 里的 gitea MCP）
//	admin  实例管理面（setup/init/actions 等需要管理员的操作）
//	review 内容评审机器人（默认 ai）
//	merge  状态评审/合并机器人（默认 merge）
//
// 与 config.json 的分工：config.json 描述「管理哪些实例与仓库」，凭据只描述「以谁
// 的身份、用哪条令牌访问」。凭据独立成文件（0600），便于轮换、审计与按账号隔离。
package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"assistant/internal/instances"
)

// 用途常量。新用途必须在这里登记，避免拼写漂移。
const (
	PurposeMCP    = "mcp"
	PurposeAdmin  = "admin"
	PurposeReview = "review"
	PurposeMerge  = "merge"
)

// MCPScopes 返回登录派生的 MCP 个人令牌权限集：仓库/Issue 读写 + 读取自身账号
// （身份校验用）。刻意不含 organization/package——MCP 工具面用不到。
//
// 每次返回新切片：调用方会把它交给排序/序列化，共享同一个 backing array 会让
// 「保存一次凭据」意外改掉全局定义。
func MCPScopes() []string {
	return []string{"read:repository", "write:repository", "read:issue", "write:issue", "read:user"}
}

// AdminScopes 返回登录为管理员派生的管理令牌权限集：仓库读写（分支保护/协作者/
// Actions secret）、Issue 读写（标签）、读取自身账号，以及管理 API（setup 创建
// 机器人账号）。管理令牌只在账号本身是实例管理员时派生。
func AdminScopes() []string {
	return []string{
		"read:repository", "write:repository",
		"read:issue", "write:issue",
		"read:user", "write:admin",
	}
}

// BotScopes 返回机器人账号（review/merge）令牌的权限集：仓库读写（分支/协作者/
// 合并）、Issue 读写（标签、评论、PR review）与读取自身账号。
func BotScopes() []string {
	return []string{"read:repository", "write:repository", "read:issue", "write:issue", "read:user"}
}

// 令牌来源，用于诊断（Credential.Source 的取值）。
const (
	// SourceLogin 表示登录按身份派生（含轮换重建）。
	SourceLogin = "login"
	// SourceSetup 表示 setup 为机器人账号创建。
	SourceSetup = "setup"
)

// CurrentVersion 是文件格式版本；升级格式时递增并在 Load 中做迁移。
const CurrentVersion = 1

// File 是 credentials.json 的根。
type File struct {
	Version int `json:"version"`
	// Identity 是每个站点当前登录的身份：一次登录确定一个（换账号即替换）。
	Identity []Identity `json:"identity,omitempty"`
	// Credentials 按 (host, user, purpose) 唯一。
	Credentials []Credential `json:"credentials,omitempty"`
}

// Identity 是某站点当前登录账号的身份事实。
type Identity struct {
	Host       string `json:"host"`
	User       string `json:"user"`
	IsAdmin    bool   `json:"is_admin"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

// Credential 是一条用途令牌。
type Credential struct {
	Host    string `json:"host"`
	User    string `json:"user"`
	Purpose string `json:"purpose"`
	// Token 是令牌明文。只有创建时能从站点读到一次，之后靠 LastEight 比对。
	Token     string   `json:"token,omitempty"`
	TokenName string   `json:"token_name,omitempty"`
	LastEight string   `json:"last_eight,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	// Source 说明令牌来源：login（登录派生）或 setup（机器人账号）。
	Source    string `json:"source,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// PathFor 决定凭据文件落点：与 config.json 同目录（一次登录对应一份运行配置），
// 无配置路径时退回标准配置目录。ASSISTANT_CREDENTIALS 可显式覆盖。
func PathFor(configPath string) (string, error) {
	if override := strings.TrimSpace(os.Getenv("ASSISTANT_CREDENTIALS")); override != "" {
		return override, nil
	}
	if path := strings.TrimSpace(configPath); path != "" {
		return filepath.Join(filepath.Dir(path), "credentials.json"), nil
	}
	dir, err := instances.DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// Load 读取凭据文件；文件不存在返回空库（不是错误）。内容损坏时报错，不静默丢弃
// 凭据。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{Version: CurrentVersion}, nil
		}
		return nil, err
	}
	file := &File{}
	if err := json.Unmarshal(data, file); err != nil {
		return nil, fmt.Errorf("解析凭据文件 %s: %w", path, err)
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return nil, fmt.Errorf("凭据文件 %s: %w", path, err)
	}
	return file, nil
}

// Save 原子写入凭据文件（0600）：临时文件 + rename，避免半截文件被读到。
func Save(path string, file *File) error {
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// Empty 表示没有任何身份与凭据：调用方据此避免写出空文件。
func (f *File) Empty() bool {
	return f == nil || (len(f.Identity) == 0 && len(f.Credentials) == 0)
}

// Normalize 补齐版本、规范化 host、去掉重复项。
func (f *File) Normalize() {
	if f.Version == 0 {
		f.Version = CurrentVersion
	}
	for index := range f.Identity {
		f.Identity[index].Host = NormalizeHost(f.Identity[index].Host)
	}
	sort.SliceStable(f.Identity, func(i, j int) bool { return f.Identity[i].Host < f.Identity[j].Host })
	dedupedIdentity := f.Identity[:0]
	for _, identity := range f.Identity {
		if n := len(dedupedIdentity); n > 0 && dedupedIdentity[n-1].Host == identity.Host {
			dedupedIdentity[n-1] = identity
			continue
		}
		dedupedIdentity = append(dedupedIdentity, identity)
	}
	f.Identity = dedupedIdentity

	for index := range f.Credentials {
		credential := &f.Credentials[index]
		credential.Host = NormalizeHost(credential.Host)
		credential.User = strings.TrimSpace(credential.User)
		credential.Purpose = strings.TrimSpace(credential.Purpose)
		if len(credential.Scopes) > 0 {
			sort.Strings(credential.Scopes)
		}
	}
	sort.SliceStable(f.Credentials, func(i, j int) bool {
		return credentialKey(f.Credentials[i]) < credentialKey(f.Credentials[j])
	})
	deduped := f.Credentials[:0]
	for _, credential := range f.Credentials {
		if n := len(deduped); n > 0 && credentialKey(deduped[n-1]) == credentialKey(credential) {
			deduped[n-1] = credential
			continue
		}
		deduped = append(deduped, credential)
	}
	f.Credentials = deduped
}

// Validate 检查索引键完整且唯一。
func (f *File) Validate() error {
	for _, identity := range f.Identity {
		if identity.Host == "" || identity.User == "" {
			return fmt.Errorf("identity 缺少 host 或 user")
		}
	}
	seen := make(map[string]bool, len(f.Credentials))
	for _, credential := range f.Credentials {
		if credential.Host == "" || credential.User == "" {
			return fmt.Errorf("credential 缺少 host 或 user（purpose=%s）", credential.Purpose)
		}
		if !KnownPurpose(credential.Purpose) {
			return fmt.Errorf("未知 purpose %q（支持 %s）", credential.Purpose, strings.Join(Purposes(), "、"))
		}
		if strings.TrimSpace(credential.Token) == "" {
			return fmt.Errorf("credential %s@%s (%s) 缺少令牌", credential.User, credential.Host, credential.Purpose)
		}
		key := credentialKey(credential)
		if seen[key] {
			return fmt.Errorf("credential 重复：%s@%s (%s)", credential.User, credential.Host, credential.Purpose)
		}
		seen[key] = true
	}
	return nil
}

// Purposes 返回已知用途（诊断/错误信息用）。
func Purposes() []string {
	return []string{PurposeMCP, PurposeAdmin, PurposeReview, PurposeMerge}
}

// KnownPurpose 判断 purpose 是否已登记。
func KnownPurpose(purpose string) bool {
	for _, known := range Purposes() {
		if purpose == known {
			return true
		}
	}
	return false
}

// NormalizeHost 统一站点比较口径：去尾斜杠、转小写。
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(host), "/"))
}

// SetIdentity 记录/替换某站点的登录身份（一个站点同时只有一个当前身份）。
func (f *File) SetIdentity(identity Identity) {
	identity.Host = NormalizeHost(identity.Host)
	identity.User = strings.TrimSpace(identity.User)
	if identity.VerifiedAt == "" {
		identity.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for index := range f.Identity {
		if f.Identity[index].Host == identity.Host {
			f.Identity[index] = identity
			return
		}
	}
	f.Identity = append(f.Identity, identity)
}

// IdentityFor 返回站点当前登录身份。
func (f *File) IdentityFor(host string) (Identity, bool) {
	if f == nil {
		return Identity{}, false
	}
	host = NormalizeHost(host)
	for _, identity := range f.Identity {
		if NormalizeHost(identity.Host) == host {
			return identity, true
		}
	}
	return Identity{}, false
}

// SetCredential 写入/替换一条用途令牌。
func (f *File) SetCredential(credential Credential) {
	credential.Host = NormalizeHost(credential.Host)
	credential.User = strings.TrimSpace(credential.User)
	credential.Purpose = strings.TrimSpace(credential.Purpose)
	// 复制 Scopes：Normalize 会就地排序，不能改动调用方持有的切片
	credential.Scopes = append([]string(nil), credential.Scopes...)
	if credential.CreatedAt == "" {
		credential.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for index := range f.Credentials {
		if credentialKey(f.Credentials[index]) == credentialKey(credential) {
			f.Credentials[index] = credential
			return
		}
	}
	f.Credentials = append(f.Credentials, credential)
}

// CredentialForUser 返回指定账号某用途的令牌。
func (f *File) CredentialForUser(host, user, purpose string) (Credential, bool) {
	if f == nil {
		return Credential{}, false
	}
	want := credentialKey(Credential{Host: host, User: user, Purpose: purpose})
	for _, credential := range f.Credentials {
		if credentialKey(credential) == want {
			return credential, true
		}
	}
	return Credential{}, false
}

// CredentialFor 返回站点某用途的令牌。同一用途在该站点存在多个账号时返回错误，
// 要求调用方指定账号——不猜身份。
func (f *File) CredentialFor(host, purpose string) (Credential, bool, error) {
	if f == nil {
		return Credential{}, false, nil
	}
	host = NormalizeHost(host)
	var matches []Credential
	for _, credential := range f.Credentials {
		if NormalizeHost(credential.Host) == host && credential.Purpose == purpose {
			matches = append(matches, credential)
		}
	}
	switch len(matches) {
	case 0:
		return Credential{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		users := make([]string, 0, len(matches))
		for _, match := range matches {
			users = append(users, "@"+match.User)
		}
		sort.Strings(users)
		return Credential{}, false, fmt.Errorf(
			"站点 %s 有多个 %s 凭据（%s），请指定账号", host, purpose, strings.Join(users, "、"))
	}
}

// CredentialForIdentity 返回站点当前登录身份在指定用途上的令牌；没有身份记录或
// 该身份没有这条令牌时退回 CredentialFor（唯一匹配；同用途多账号才要求指定）。
// 身份是「这台站点现在以谁登录」的事实，因此多账号并存时它是有依据的裁决者。
func (f *File) CredentialForIdentity(host, purpose string) (Credential, bool, error) {
	if identity, ok := f.IdentityFor(host); ok {
		if credential, ok := f.CredentialForUser(host, identity.User, purpose); ok {
			return credential, true, nil
		}
	}
	return f.CredentialFor(host, purpose)
}

// RemoveUser 删除某账号在该站点的全部凭据（不动远端令牌；只清本地记录）。
func (f *File) RemoveUser(host, user string) int {
	if f == nil {
		return 0
	}
	host, user = NormalizeHost(host), strings.TrimSpace(user)
	kept := f.Credentials[:0]
	removed := 0
	for _, credential := range f.Credentials {
		if NormalizeHost(credential.Host) == host && credential.User == user {
			removed++
			continue
		}
		kept = append(kept, credential)
	}
	f.Credentials = kept
	identities := f.Identity[:0]
	for _, identity := range f.Identity {
		if NormalizeHost(identity.Host) == host && identity.User == user {
			continue
		}
		identities = append(identities, identity)
	}
	f.Identity = identities
	return removed
}

// Users 返回站点上已登记的账号名（排序、去重）。
func (f *File) Users(host string) []string {
	if f == nil {
		return nil
	}
	host = NormalizeHost(host)
	var users []string
	seen := map[string]bool{}
	add := func(user string) {
		if user != "" && !seen[user] {
			seen[user] = true
			users = append(users, user)
		}
	}
	for _, identity := range f.Identity {
		if NormalizeHost(identity.Host) == host {
			add(identity.User)
		}
	}
	for _, credential := range f.Credentials {
		if NormalizeHost(credential.Host) == host {
			add(credential.User)
		}
	}
	sort.Strings(users)
	return users
}

// TokenName 由 (host, user, purpose) 确定性派生令牌名：同名即同一用途令牌，重登
// 复用而不是堆积。user 参与命名，保证多账号同站点互不覆盖。
func TokenName(host, user, purpose string) (string, error) {
	slug, err := instances.HostSlug(NormalizeHost(host))
	if err != nil {
		return "", err
	}
	user = strings.TrimSpace(user)
	if user == "" || !KnownPurpose(purpose) {
		return "", fmt.Errorf("派生令牌名需要账号与已登记的用途")
	}
	return "assistant-" + purpose + "-" + slug + "-" + user, nil
}

// LastEight 返回令牌末 8 位：Gitea 只回读末 8 位，用于本地复用校验。
func LastEight(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 8 {
		return token
	}
	return token[len(token)-8:]
}

func credentialKey(credential Credential) string {
	return NormalizeHost(credential.Host) + "\x00" + strings.TrimSpace(credential.User) + "\x00" +
		strings.TrimSpace(credential.Purpose)
}
