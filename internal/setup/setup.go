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

	"assistant/internal/credentials"
	"assistant/internal/instances"
)

// Options 是 setup 的输入。
type Options struct {
	Host string
	// AdminToken 是调用方从凭据库（purpose=admin）解析出的管理员令牌；setup 不
	// 自己获取凭据——登录是唯一入口。
	AdminToken        string
	Repos             []string
	ReviewerName      string
	MergerName        string
	EmailDomain       string
	RequiredApprovals int64
	CreateRepos       bool
	DryRun            bool
	// AllowAdminOverride 为真时不勾选「管理员须遵守分支保护规则」；缺省 false，
	// 即管理员（含 merger）也必须满足审批/检查门禁、不能绕过。
	AllowAdminOverride bool
	// Existing 是 config.json 里该 instance 的现状（可空）。
	Existing *instances.Instance
	// ExistingCredentials 是凭据库里现有的机器人令牌：有效则复用，不重建。
	ExistingCredentials []credentials.Credential
	Log                 func(format string, arguments ...any)
}

// Result 是 setup 的产物：实例配置（不含凭据）+ 需要写回凭据库的机器人令牌。
type Result struct {
	Instance    instances.Instance
	Credentials []credentials.Credential
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
	UserExists(ctx context.Context, name string) (bool, error)
	CreateUser(ctx context.Context, name, email string) error
	// EnsurePassword 返回账号可用密码（本次创建或管理员重置）。
	EnsurePassword(ctx context.Context, name string) (string, error)
	// ConvergeToken 收敛账号令牌为唯一一个：保留 keepToken（末 8 位匹配）
	// 或新建 tokenName，删除其余全部。review/merge 机器人都用：一个站点
	// 一个机器人账号只允许一条令牌。
	ConvergeToken(ctx context.Context, name, password, tokenName, keepToken string) (token string, created bool, err error)
	// ValidateToken 确认 token 属于 name 账号（用于复用已有令牌）。
	ValidateToken(ctx context.Context, name, token string) (bool, error)
	GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error)
	CreateRepo(ctx context.Context, fullName string) error
	AddCollaborator(ctx context.Context, fullName, user, permission string) error
	// RemoveCollaborator 移除仓库协作者（不存在时 no-op；deinit --purge 用）。
	RemoveCollaborator(ctx context.Context, fullName, user string) error
	EnsureBranchProtection(ctx context.Context, fullName string, options ProtectionOptions) error
	// DeleteBranchProtection 删除分支保护规则（不存在时 no-op；deinit --purge 用）。
	DeleteBranchProtection(ctx context.Context, fullName, branch string) error
	ReconcileLabels(ctx context.Context, fullName, reviewerToken string) error
	// SetRepoSecret / DeleteRepoSecret 写/删仓库级 Actions secret（幂等）。
	// 身份名是约定（ai/merge），无需 variable。
	SetRepoSecret(ctx context.Context, fullName, name, value string) error
	DeleteRepoSecret(ctx context.Context, fullName, name string) error
}

// ProtectionOptions 是 setup 统一写入的分支保护配置。
type ProtectionOptions struct {
	Branch            string
	RequiredApprovals int64
	// MergerName 进入合并白名单：只允许 merger 合并（自动化会签 + 合并）。
	MergerName string
	// AllowAdminOverride 为真时不勾选「管理员须遵守分支保护规则」；缺省 false
	// （勾选：身为管理员的 merger 也必须满足审批/检查门禁，不能绕过）。
	AllowAdminOverride bool
}

var accountNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Run 执行整个初始化流程：返回写回 config.json 的 instance 与写回凭据库的机器人
// 令牌（review / merge，各自 (host, 账号) 唯一）。
func Run(ctx context.Context, options Options, admin Admin) (Result, error) {
	options.applyDefaults()
	if err := options.validate(); err != nil {
		return Result{}, err
	}
	logf := options.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	login, isAdmin, err := admin.AuthenticatedUser(ctx)
	if err != nil {
		return Result{}, err
	}
	if !isAdmin {
		return Result{}, fmt.Errorf("账号 @%s 不是管理员，setup 需要管理员权限", login)
	}
	logf("管理员 @%s 校验通过", login)

	instance := instances.Instance{Host: options.Host}
	if options.Existing != nil {
		instance = *options.Existing
		instance.Host = options.Host
		instance.Repos = nil
	}

	// reviewer（ai）：单一令牌——同一站点只有一个评审主机，令牌唯一从凭据
	// 层面兜底（dispatcher 的单飞锁是进程内的）。
	reviewerToken, reviewerCreated, err := ensureAccount(
		ctx, admin, options, logf, options.ReviewerName, options.existingToken(credentials.PurposeReview),
	)
	if err != nil {
		return Result{}, err
	}
	if reviewerCreated {
		logf("已为 %s 生成访问令牌（账号下仅此一个）", options.ReviewerName)
	}

	// merger（merge）：同样是账号一个、令牌一条（(host, merge) 唯一）。仓库级
	// Actions 各自把这条令牌写进自己的 secret。
	mergerToken, mergerCreated, err := ensureAccount(
		ctx, admin, options, logf, options.MergerName, options.existingToken(credentials.PurposeMerge),
	)
	if err != nil {
		return Result{}, err
	}
	if mergerCreated {
		logf("已为 %s 生成访问令牌（账号下仅此一个）", options.MergerName)
	}

	instance.Reviewer = instances.Account{Name: options.ReviewerName}
	instance.Merger = instances.Account{Name: options.MergerName}
	instance.Repos = make([]instances.Repo, 0, len(options.Repos))

	for _, fullName := range options.Repos {
		repo := instances.Repo{Name: fullName}
		if options.Existing != nil {
			if existingRepo, ok := options.Existing.FindRepo(fullName); ok {
				repo = existingRepo
			}
		}
		if err := setupRepository(ctx, options, admin, logf, fullName, reviewerToken); err != nil {
			return Result{}, fmt.Errorf("%s: %w", fullName, err)
		}
		instance.Repos = append(instance.Repos, repo)
	}

	instance.Normalize()
	if err := instance.Validate(); err != nil {
		return Result{}, err
	}
	result := Result{Instance: instance}
	for _, bot := range []struct {
		name    string
		purpose string
		token   string
	}{
		{options.ReviewerName, credentials.PurposeReview, reviewerToken},
		{options.MergerName, credentials.PurposeMerge, mergerToken},
	} {
		if bot.token == "" {
			continue // dry-run：保留现有凭据不动
		}
		result.Credentials = append(result.Credentials, credentials.Credential{
			Host: options.Host, User: bot.name, Purpose: bot.purpose,
			Token: bot.token, TokenName: ReviewerTokenName,
			LastEight: credentials.LastEight(bot.token),
			Scopes:    credentials.BotScopes(), Source: credentials.SourceSetup,
		})
	}
	return result, nil
}

// ReviewerTokenName 是机器人账号（review/merge）的唯一令牌名。
const ReviewerTokenName = "assistant"

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

	for _, collaborator := range []struct{ name, permission string }{
		{options.ReviewerName, "write"},
		// merger 需要仓库管理员权限：合并白名单 + 分支保护读取（repo admin）。
		{options.MergerName, "admin"},
	} {
		logf("添加协作者 %s（%s）", collaborator.name, collaborator.permission)
		if !options.DryRun {
			if err := admin.AddCollaborator(ctx, fullName, collaborator.name, collaborator.permission); err != nil {
				return err
			}
		}
	}

	if info.DefaultBranch == "" || info.Empty {
		logf("仓库为空或无默认分支，跳过分支保护")
		return nil
	}
	adminOverride := "关闭（管理员也须遵守）"
	if options.AllowAdminOverride {
		adminOverride = "开启（管理员可绕过）"
	}
	logf("配置分支保护 %s（required approvals=%d，只允许 %s 合并，驳回/未回应请求均阻塞，过期批准作废，落后分支阻塞，管理员绕过 %s）",
		info.DefaultBranch, options.RequiredApprovals, options.MergerName, adminOverride)
	if !options.DryRun {
		if err := admin.EnsureBranchProtection(ctx, fullName, ProtectionOptions{
			Branch:             info.DefaultBranch,
			RequiredApprovals:  options.RequiredApprovals,
			MergerName:         options.MergerName,
			AllowAdminOverride: options.AllowAdminOverride,
		}); err != nil {
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
	token, created, err = admin.ConvergeToken(ctx, name, password, ReviewerTokenName, keep)
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

// existingToken 返回凭据库里该用途现有令牌（用于「有效则复用」判断）。
func (o Options) existingToken(purpose string) string {
	for _, credential := range o.ExistingCredentials {
		if credential.Purpose == purpose {
			return credential.Token
		}
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

func (o Options) email(name string) string {
	return name + "@" + o.EmailDomain
}

func (o Options) validate() error {
	parsed, err := url.Parse(o.Host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("host 必须是绝对 HTTP(S) URL：%q", o.Host)
	}
	if o.AdminToken == "" {
		return fmt.Errorf("缺少管理员令牌：先用管理员账号 assistant login %s", o.Host)
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
