// Package setup 实现 `assistant setup`：给定一台 Gitea 的管理员凭据与仓库
// 清单，自动完成实例初始化——
//
//  1. 复用/创建 reviewer（默认 ai）与 merger（默认 merge）两个机器人账号；
//  2. 为它们生成访问令牌（已配置且仍有效的令牌直接复用）；
//  3. 把两个账号加为仓库协作者（write），并按统一口径补齐标签体系；
//  4. 在默认分支上配置分支保护（required approvals、驳回阻塞、过期批准作废、
//     落后分支阻塞），使「评审 → 批准 → 会签 → 自动合并」闭环成立；
//  5. 返回可写回 config.json 的 instance 配置。
//
// 全流程幂等：重复执行不重复建号/建令牌，只收敛配置。
package setup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"assistant/internal/instances"
)

// Options 是 setup 的输入。
type Options struct {
	Host              string
	AdminToken        string
	AdminUser         string
	AdminPassword     string
	OAuth             *OAuthOptions
	Repos             []string
	ReviewerName      string
	MergerName        string
	EmailDomain       string
	RequiredApprovals int64
	CreateRepos       bool
	DryRun            bool
	// Existing 是 config.json 里该 instance 的现状（可空），用于复用有效令牌。
	Existing *instances.Instance
	Log      func(format string, arguments ...any)
}

// RepoInfo 是 setup 关心的仓库元数据。
type RepoInfo struct {
	DefaultBranch string
	Empty         bool
}

// Admin 是 setup 所需的高权限操作面；giteaAdmin 是真实实现。
type Admin interface {
	// AuthenticatedUser 返回当前管理员登录名并确认管理员身份。
	AuthenticatedUser(ctx context.Context) (login string, isAdmin bool, err error)
	// AdminToken 返回实际生效的管理员令牌（用户提供、OAuth 换取或新生成的）；
	// dry-run 且仅有账号密码时可能为空。
	AdminToken() string
	// PersistentToken 返回可写入配置长期使用的管理员令牌；OAuth 令牌会过期，
	// 返回空表示不应落盘。
	PersistentToken() string
	// AdminOAuth 返回 OAuth 刷新凭据（可落盘）；非 OAuth 登录时为 nil。
	AdminOAuth() *instances.OAuthCredential
	UserExists(ctx context.Context, name string) (bool, error)
	CreateUser(ctx context.Context, name, email string) error
	// EnsurePassword 返回账号可用密码（本次创建或管理员重置）。
	EnsurePassword(ctx context.Context, name string) (string, error)
	// ConvergeToken 把账号令牌收敛为唯一一个：保留 keepToken（末 8 位匹配）
	// 或新建，删除其余全部。
	ConvergeToken(ctx context.Context, name, password, keepToken string) (token string, created bool, err error)
	// ValidateToken 确认 token 属于 name 账号（用于复用已有令牌）。
	ValidateToken(ctx context.Context, name, token string) (bool, error)
	GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error)
	CreateRepo(ctx context.Context, fullName string) error
	AddCollaborator(ctx context.Context, fullName, user string) error
	EnsureBranchProtection(ctx context.Context, fullName, branch string, requiredApprovals int64) error
	ReconcileLabels(ctx context.Context, fullName, reviewerToken string) error
	// SetRepoVariable / SetRepoSecret 写仓库级 Actions 配置（幂等覆盖）。
	SetRepoVariable(ctx context.Context, fullName, name, value string) error
	SetRepoSecret(ctx context.Context, fullName, name, value string) error
}

var accountNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Run 执行整个初始化流程并返回写回 config.json 的 instance。
func Run(ctx context.Context, options Options, admin Admin) (instances.Instance, error) {
	options.applyDefaults()
	if err := options.validate(); err != nil {
		return instances.Instance{}, err
	}
	logf := options.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	login, isAdmin, err := admin.AuthenticatedUser(ctx)
	if err != nil {
		return instances.Instance{}, err
	}
	if !isAdmin {
		return instances.Instance{}, fmt.Errorf("账号 @%s 不是管理员，setup 需要管理员权限", login)
	}
	logf("管理员 @%s 校验通过", login)

	instance := instances.Instance{Host: options.Host}
	if options.Existing != nil {
		instance = *options.Existing
		instance.Host = options.Host
		instance.Repos = nil
	}

	reviewerToken, reviewerCreated, err := ensureAccount(
		ctx, admin, options, logf, options.ReviewerName, existingToken(instance.Reviewer, options.ReviewerName),
	)
	if err != nil {
		return instances.Instance{}, err
	}
	mergerToken, mergerCreated, err := ensureAccount(
		ctx, admin, options, logf, options.MergerName, existingToken(instance.Merger, options.MergerName),
	)
	if err != nil {
		return instances.Instance{}, err
	}
	if reviewerCreated || mergerCreated {
		logf("令牌已生成（写入 config 后请妥善保管）")
	}

	// 管理令牌：本次实际生效的优先（用户显式提供或由账号密码新生成），
	// 其次保留 config 里的旧值。OAuth 令牌会过期（PersistentToken 为空），
	// 只用于本次 setup，不落盘。
	instance.AdminToken = admin.PersistentToken()
	if instance.AdminToken == "" {
		instance.AdminToken = options.existingAdminToken()
	}
	if instance.AdminToken == "" {
		instance.AdminToken = options.AdminToken
	}
	instance.AdminOAuth = admin.AdminOAuth()
	instance.Reviewer = instances.Account{Name: options.ReviewerName, Token: reviewerToken}
	instance.Merger = instances.Account{Name: options.MergerName, Token: mergerToken}
	instance.Repos = make([]instances.Repo, 0, len(options.Repos))

	for _, fullName := range options.Repos {
		repo := instances.Repo{Name: fullName}
		if options.Existing != nil {
			if existing, ok := options.Existing.FindRepo(fullName); ok {
				repo = existing
			}
		}
		if err := setupRepository(ctx, options, admin, logf, fullName, reviewerToken); err != nil {
			return instances.Instance{}, fmt.Errorf("%s: %w", fullName, err)
		}
		instance.Repos = append(instance.Repos, repo)
	}

	instance.Normalize()
	if err := instance.Validate(); err != nil {
		return instances.Instance{}, err
	}
	return instance, nil
}

func setupRepository(
	ctx context.Context,
	options Options,
	admin Admin,
	logf func(string, ...any),
	fullName, reviewerToken string,
) error {
	info, exists, err := admin.GetRepo(ctx, fullName)
	if err != nil {
		return err
	}
	if !exists {
		if !options.CreateRepos {
			return fmt.Errorf("仓库不存在（如需自动创建请加 --create-repos）")
		}
		logf("创建仓库 %s", fullName)
		if !options.DryRun {
			if err := admin.CreateRepo(ctx, fullName); err != nil {
				return err
			}
			if info, exists, err = admin.GetRepo(ctx, fullName); err != nil {
				return err
			} else if !exists {
				return fmt.Errorf("创建后仍读不到仓库")
			}
		}
	}

	for _, bot := range []string{options.ReviewerName, options.MergerName} {
		logf("添加协作者 %s（write）", bot)
		if !options.DryRun {
			if err := admin.AddCollaborator(ctx, fullName, bot); err != nil {
				return err
			}
		}
	}

	if info.DefaultBranch == "" || info.Empty {
		logf("仓库为空或无默认分支，跳过分支保护")
		return nil
	}
	logf("配置分支保护 %s（required approvals=%d，驳回阻塞，过期批准作废，落后分支阻塞）",
		info.DefaultBranch, options.RequiredApprovals)
	if !options.DryRun {
		if err := admin.EnsureBranchProtection(ctx, fullName, info.DefaultBranch, options.RequiredApprovals); err != nil {
			return err
		}
	}

	logf("补齐规范标签体系（与 sync 同一口径）")
	if !options.DryRun {
		if err := admin.ReconcileLabels(ctx, fullName, reviewerToken); err != nil {
			return err
		}
	}
	return nil
}

// ensureAccount 保证账号与唯一令牌可用：账号缺失则创建；令牌以「保留现有
// 有效令牌或新建，删除其余全部」的方式收敛——同一账号只允许一个令牌，从
// 凭据层面保证同一站点只有一个评审主机。dry-run 下只输出计划。
func ensureAccount(
	ctx context.Context,
	admin Admin,
	options Options,
	logf func(string, ...any),
	name, existingToken string,
) (token string, created bool, err error) {
	valid := false
	if existingToken != "" {
		ok, validateErr := admin.ValidateToken(ctx, name, existingToken)
		switch {
		case validateErr != nil:
			logf("%s 的现有令牌校验失败（%v），将重建", name, validateErr)
		case ok:
			valid = true
			logf("复用 %s 的现有令牌", name)
		default:
			logf("%s 的现有令牌已失效，将重建", name)
		}
	}
	exists, err := admin.UserExists(ctx, name)
	if err != nil {
		return "", false, err
	}
	if !exists {
		logf("创建账号 %s（邮箱 %s，随机密码不落盘）", name, options.email(name))
		if !options.DryRun {
			if err := admin.CreateUser(ctx, name, options.email(name)); err != nil {
				return "", false, err
			}
		}
	} else {
		logf("账号 %s 已存在", name)
	}
	if options.DryRun {
		if valid {
			logf("dry-run：保留 %s 的现有令牌，删除账号下其余令牌", name)
		} else {
			logf("dry-run：为 %s 生成新令牌，删除账号下其余令牌", name)
		}
		return existingToken, false, nil
	}
	password, err := admin.EnsurePassword(ctx, name)
	if err != nil {
		return "", false, fmt.Errorf("准备 %s 的密码: %w", name, err)
	}
	keep := ""
	if valid {
		keep = existingToken
	}
	token, created, err = admin.ConvergeToken(ctx, name, password, keep)
	if err != nil {
		return "", false, err
	}
	if created {
		logf("已为 %s 生成访问令牌（账号下仅此一个）", name)
	} else {
		logf("%s 的令牌已收敛为唯一一个", name)
	}
	return token, created, nil
}

func existingToken(account instances.Account, name string) string {
	if account.Name == name {
		return account.Token
	}
	return ""
}

func (o *Options) applyDefaults() {
	o.Host = strings.TrimRight(strings.TrimSpace(o.Host), "/")
	if o.ReviewerName == "" {
		o.ReviewerName = instances.DefaultReviewerName
	}
	if o.MergerName == "" {
		o.MergerName = instances.DefaultMergerName
	}
	if o.RequiredApprovals == 0 {
		o.RequiredApprovals = 2
	}
	if o.EmailDomain == "" {
		o.EmailDomain = DeriveEmailDomain(o.Host)
	}
}

func (o Options) existingAdminToken() string {
	if o.Existing != nil {
		return o.Existing.AdminToken
	}
	return ""
}

func (o Options) email(name string) string {
	return name + "@" + o.EmailDomain
}

func (o Options) validate() error {
	parsed, err := url.Parse(o.Host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("host 必须是绝对 HTTP(S) URL：%q", o.Host)
	}
	if o.AdminToken == "" && o.OAuth == nil && (o.AdminUser == "" || o.AdminPassword == "") {
		return fmt.Errorf("缺少管理员凭据：--admin-token / --admin-token-file / --oauth，或 --admin-user/--admin-password")
	}
	for _, name := range []string{o.ReviewerName, o.MergerName} {
		if !accountNamePattern.MatchString(name) {
			return fmt.Errorf("账号名 %q 非法（仅字母数字，可含 . _ -）", name)
		}
	}
	if o.ReviewerName == o.MergerName {
		return fmt.Errorf("reviewer 与 merger 不能是同一账号（%s）", o.ReviewerName)
	}
	for _, fullName := range o.Repos {
		if _, _, err := instances.ParseRepoName(fullName); err != nil {
			return err
		}
	}
	return nil
}

// DeriveEmailDomain 从站点 URL 推导机器人邮箱域名；IP/localhost 回退。
func DeriveEmailDomain(host string) string {
	parsed, err := url.Parse(host)
	if err != nil {
		return "assistant.local"
	}
	hostname := parsed.Hostname()
	if hostname == "" || hostname == "localhost" || net.ParseIP(hostname) != nil {
		return "assistant.local"
	}
	return hostname
}

// RandomPassword 生成机器人账号的随机初始密码（不落盘；API 用令牌）。
func RandomPassword() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
