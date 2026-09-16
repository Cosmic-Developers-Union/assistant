package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gitea "gitea.dev/sdk"

	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/status"
)

const (
	// 机器人令牌的 scope 见 credentials.BotScopes()。
	httpTimeout = 30 * time.Second
)

// GiteaClient 是 Admin 的真实实现别名：init（dev 令牌）与 setup（admin 令牌）
// 共用同一操作面，权限差异由服务端与调用方校验保证。
type GiteaClient = giteaAdmin

// giteaAdmin 是 Admin 的真实实现：读操作走 raw REST，写操作走 Gitea SDK。
// 管理员令牌由调用方从凭据库（purpose=admin）解析后传入——setup 不自己获取凭据。
type giteaAdmin struct {
	host   string
	token  string
	http   *http.Client
	sdk    *gitea.Client
	dryRun bool
	log    func(string, ...any)
	// passwords 记录本次创建的机器人账号随机密码，用于令牌创建失败时以
	// Basic Auth 回退（不写盘）。
	passwords map[string]string
	// login 是 NewRepoClient 校验时记录的令牌归属账号。
	login string
}

// NewAdmin 构造高权限操作面：只接受管理员令牌（assistant login 写入凭据库的
// purpose=admin），并在线校验该令牌确实是实例管理员。
func NewAdmin(ctx context.Context, options Options) (*giteaAdmin, error) {
	options.applyDefaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	logf := options.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	httpClient := &http.Client{Timeout: httpTimeout}
	admin := &giteaAdmin{
		host:      options.Host,
		token:     options.AdminToken,
		http:      httpClient,
		dryRun:    options.DryRun,
		log:       logf,
		passwords: map[string]string{},
	}
	login, isAdmin, err := admin.AuthenticatedUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("校验管理员令牌: %w", err)
	}
	if !isAdmin {
		return nil, fmt.Errorf("账号 @%s 不是管理员，setup 需要管理员权限", login)
	}
	admin.login = login
	sdk, err := gitea.NewClient(
		admin.host,
		gitea.SetToken(admin.token),
		gitea.SetHTTPClient(httpClient),
		gitea.SetUserAgent("assistant-setup/1"),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 Gitea 客户端: %w", err)
	}
	admin.sdk = sdk
	return admin, nil
}

// NewRepoClient 构造仓库级操作面：接受仓库管理员（dev）自己的令牌（凭据库
// purpose=mcp），不做站点管理员校验。init 子命令（协作者、分支保护）用它；
// 站点级初始化仍走 NewAdmin（admin 令牌）。
func NewRepoClient(ctx context.Context, host, token string, logf func(string, ...any)) (*giteaAdmin, error) {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if parsed, err := url.Parse(host); err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("host 必须是绝对 HTTP(S) URL：%q", host)
	}
	if token == "" {
		return nil, fmt.Errorf("缺少访问令牌：先 assistant login %s", host)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	httpClient := &http.Client{Timeout: httpTimeout}
	client := &giteaAdmin{
		host:      host,
		token:     token,
		http:      httpClient,
		log:       logf,
		passwords: map[string]string{},
	}
	login, _, err := client.AuthenticatedUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("校验令牌: %w", err)
	}
	client.login = login
	sdk, err := gitea.NewClient(
		client.host,
		gitea.SetToken(client.token),
		gitea.SetHTTPClient(httpClient),
		gitea.SetUserAgent("assistant-init/1"),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 Gitea 客户端: %w", err)
	}
	client.sdk = sdk
	return client, nil
}

// Login 返回当前令牌归属的账号名（NewRepoClient 校验时记录）。
func (a *giteaAdmin) Login() string { return a.login }

// Token 返回构造时使用的访问令牌（供复用同一令牌的其他客户端，如标签管理）。
func (a *giteaAdmin) Token() string { return a.token }

func (a *giteaAdmin) AuthenticatedUser(ctx context.Context) (string, bool, error) {
	var payload struct {
		Login   string `json:"login"`
		IsAdmin bool   `json:"is_admin"`
	}
	if _, err := a.do(ctx, http.MethodGet, "/api/v1/user", a.auth(), nil, &payload); err != nil {
		return "", false, err
	}
	if payload.Login == "" {
		return "", false, fmt.Errorf("Gitea 返回缺 login")
	}
	return payload.Login, payload.IsAdmin, nil
}

func (a *giteaAdmin) UserExists(ctx context.Context, name string) (bool, error) {
	_, err := a.do(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(name), a.auth(), nil, nil)
	if isHTTPStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (a *giteaAdmin) CreateUser(ctx context.Context, name, email string) error {
	if a.sdk == nil {
		return fmt.Errorf("缺少管理员令牌，无法创建账号")
	}
	password, err := RandomPassword()
	if err != nil {
		return err
	}
	mustChange := false
	if _, _, err := a.sdk.Admin.CreateUser(ctx, gitea.CreateUserOption{
		Username:           name,
		Email:              email,
		Password:           password,
		MustChangePassword: &mustChange,
	}); err != nil {
		return fmt.Errorf("创建账号 %s: %w", name, err)
	}
	a.passwords[name] = password
	return nil
}

// EnsurePassword 返回机器人账号的可用密码：本次创建的密码，或由管理员重置
// （机器人不登录 UI，重置无副作用）。
func (a *giteaAdmin) EnsurePassword(ctx context.Context, name string) (string, error) {
	if password := a.passwords[name]; password != "" {
		return password, nil
	}
	return a.resetPassword(ctx, name)
}

// tokenInfo 是账号令牌的只读视图（令牌值不可回读，只能按末 8 位匹配）。
type tokenInfo struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	TokenLastEight string `json:"token_last_eight"`
}

// ConvergeToken 保证名为 tokenName 的本工具令牌可用：keepToken（按末 8 位匹配）
// 命中则直接复用；否则只删除名为 tokenName 的旧条目后新建。账号下**其他命名的
// 令牌一律不动**——机器人账号可能同时被人工使用，setup 不做服务端密钥清理。
func (a *giteaAdmin) ConvergeToken(
	ctx context.Context,
	name, password, tokenName, keepToken string,
) (token string, created bool, err error) {
	tokens, err := a.listTokensBasic(ctx, name, password)
	if err != nil {
		return "", false, fmt.Errorf("列出 %s 的令牌: %w", name, err)
	}
	keepID := matchToken(tokens, keepToken)
	deleted := 0
	for _, item := range tokens {
		if item.ID == keepID || item.Name != tokenName {
			continue
		}
		if err := a.deleteTokenBasic(ctx, name, password, item.ID); err != nil {
			return "", false, fmt.Errorf("删除 %s 的本工具令牌 %s: %w", name, item.Name, err)
		}
		deleted++
	}
	if keepID != 0 {
		token = keepToken
	} else {
		token, err = a.createTokenBasic(ctx, name, password, tokenName)
		if err != nil {
			return "", false, err
		}
		created = true
	}
	if deleted > 0 {
		a.log("已清理 %s 的 %d 个旧的本工具令牌（其他命名的令牌未触碰）", name, deleted)
	}
	return token, created, nil
}

// matchToken 按末 8 位找到 keepToken 在令牌列表中的条目；找不到返回 0。
func matchToken(tokens []tokenInfo, token string) int64 {
	if token == "" {
		return 0
	}
	lastEight := tokenLastEight(token)
	for _, item := range tokens {
		if item.TokenLastEight == lastEight {
			return item.ID
		}
	}
	return 0
}

// createTokenBasic 以机器人自己的 Basic Auth 建一个指定名称的令牌。
// Gitea 的建令牌端点只接受 Basic Auth：管理员令牌调不了。
func (a *giteaAdmin) createTokenBasic(ctx context.Context, name, password, tokenName string) (string, error) {
	body := map[string]any{"name": tokenName, "scopes": credentials.BotScopes()}
	var payload struct {
		Token string `json:"sha1"`
	}
	path := "/api/v1/users/" + url.PathEscape(name) + "/tokens"
	if _, err := a.do(ctx, http.MethodPost, path, requestAuth{user: name, password: password}, body, &payload); err != nil {
		return "", fmt.Errorf("为 %s 创建令牌: %w", name, err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("为 %s 创建令牌: Gitea 未返回令牌", name)
	}
	return payload.Token, nil
}

func (a *giteaAdmin) listTokensBasic(ctx context.Context, name, password string) ([]tokenInfo, error) {
	var result []tokenInfo
	for page := 1; ; page++ {
		var batch []tokenInfo
		path := fmt.Sprintf("/api/v1/users/%s/tokens?page=%d&limit=50", url.PathEscape(name), page)
		if _, err := a.do(ctx, http.MethodGet, path, requestAuth{user: name, password: password}, nil, &batch); err != nil {
			return nil, err
		}
		result = append(result, batch...)
		if len(batch) < 50 {
			return result, nil
		}
	}
}

func (a *giteaAdmin) deleteTokenBasic(ctx context.Context, name, password string, id int64) error {
	path := fmt.Sprintf("/api/v1/users/%s/tokens/%d", url.PathEscape(name), id)
	if _, err := a.do(ctx, http.MethodDelete, path, requestAuth{user: name, password: password}, nil, nil); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// tokenLastEight 取令牌末 8 位（与 Gitea 的 token_last_eight 对齐）。
func tokenLastEight(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[len(token)-8:]
}

// Collaborator 是仓库协作者的只读视图。
type Collaborator struct {
	Name       string
	Permission string // admin / write / read
}

// ListCollaborators 列出仓库协作者及其权限（需要仓库管理员权限）。列表端点的
// 权限是对象（{"admin","push","pull"} 布尔），这里归一化为 admin/write/read。
func (a *giteaAdmin) ListCollaborators(ctx context.Context, fullName string) ([]Collaborator, error) {
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return nil, err
	}
	var result []Collaborator
	for page := 1; ; page++ {
		var batch []struct {
			Login       string `json:"login"`
			Permissions *struct {
				Admin bool `json:"admin"`
				Push  bool `json:"push"`
				Pull  bool `json:"pull"`
			} `json:"permissions"`
		}
		path := fmt.Sprintf("/api/v1/repos/%s/%s/collaborators?page=%d&limit=50",
			url.PathEscape(owner), url.PathEscape(name), page)
		if _, err := a.do(ctx, http.MethodGet, path, a.auth(), nil, &batch); err != nil {
			return nil, fmt.Errorf("列出 %s 的协作者: %w", fullName, err)
		}
		for _, item := range batch {
			permission := "read"
			switch {
			case item.Permissions == nil:
				permission = ""
			case item.Permissions.Admin:
				permission = "admin"
			case item.Permissions.Push:
				permission = "write"
			}
			result = append(result, Collaborator{Name: item.Login, Permission: permission})
		}
		if len(batch) < 50 {
			return result, nil
		}
	}
}

// ListAllRepos 列出实例上的全部仓库（站点管理员端点）。
func (a *giteaAdmin) ListAllRepos(ctx context.Context) ([]string, error) {
	var result []string
	for page := 1; ; page++ {
		var batch []struct {
			FullName string `json:"full_name"`
		}
		path := fmt.Sprintf("/api/v1/admin/repos?page=%d&limit=50", page)
		if _, err := a.do(ctx, http.MethodGet, path, a.auth(), nil, &batch); err != nil {
			return nil, fmt.Errorf("列出实例仓库: %w", err)
		}
		for _, item := range batch {
			result = append(result, item.FullName)
		}
		if len(batch) < 50 {
			return result, nil
		}
	}
}

// SetRepoSecret 写仓库级 Actions secret（PUT 幂等覆盖；secret 值只写不可读，
// 无法比对，只能每次覆盖）。
func (a *giteaAdmin) SetRepoSecret(ctx context.Context, fullName, name, value string) error {
	owner, repository, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/secrets/%s",
		url.PathEscape(owner), url.PathEscape(repository), url.PathEscape(name))
	if _, err := a.do(ctx, http.MethodPut, path, a.auth(), map[string]any{"data": value}, nil); err != nil {
		return fmt.Errorf("写 %s 的 Actions secret %s: %w", fullName, name, err)
	}
	return nil
}

// resetPassword 由管理员重置机器人账号密码（仅用于以 Basic Auth 管理它自己
// 的令牌；机器人不登录 UI，重置无副作用）。
func (a *giteaAdmin) resetPassword(ctx context.Context, name string) (string, error) {
	if a.sdk == nil {
		return "", fmt.Errorf("缺少管理员令牌")
	}
	password, err := RandomPassword()
	if err != nil {
		return "", err
	}
	mustChange := false
	// login_name 是 Gitea 1.22 PATCH /admin/users 的必填绑定字段（本地账号
	// 与用户名一致），缺失会 422 [LoginName]: Required。
	if _, err := a.sdk.Admin.EditUser(ctx, name, gitea.EditUserOption{
		LoginName:          name,
		Password:           password,
		MustChangePassword: &mustChange,
	}); err != nil {
		return "", err
	}
	a.passwords[name] = password
	return password, nil
}

func (a *giteaAdmin) ValidateToken(ctx context.Context, name, token string) (bool, error) {
	var payload struct {
		Login string `json:"login"`
	}
	_, err := a.do(ctx, http.MethodGet, "/api/v1/user", requestAuth{token: token}, nil, &payload)
	if isHTTPStatus(err, http.StatusUnauthorized) || isHTTPStatus(err, http.StatusForbidden) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return payload.Login == name, nil
}

func (a *giteaAdmin) GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error) {
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return RepoInfo{}, false, err
	}
	var payload struct {
		DefaultBranch string `json:"default_branch"`
		Empty         bool   `json:"empty"`
	}
	_, err = a.do(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), a.auth(), nil, &payload)
	if isHTTPStatus(err, http.StatusNotFound) {
		return RepoInfo{}, false, nil
	}
	if err != nil {
		return RepoInfo{}, false, err
	}
	return RepoInfo{DefaultBranch: payload.DefaultBranch, Empty: payload.Empty}, true, nil
}

func (a *giteaAdmin) CreateRepo(ctx context.Context, fullName string) error {
	if a.sdk == nil {
		return fmt.Errorf("缺少管理员令牌，无法创建仓库")
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	option := gitea.CreateRepoOption{Name: name, Private: true, AutoInit: true, DefaultBranch: "main"}
	isOrg := false
	if _, err := a.do(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(owner), a.auth(), nil, nil); err == nil {
		isOrg = true
	} else if !isHTTPStatus(err, http.StatusNotFound) {
		return fmt.Errorf("探测组织 %s: %w", owner, err)
	}
	if isOrg {
		if _, _, err := a.sdk.Repositories.CreateOrgRepo(ctx, owner, option); err != nil {
			return fmt.Errorf("创建组织仓库 %s: %w", fullName, err)
		}
		return nil
	}
	if _, _, err := a.sdk.Admin.CreateRepo(ctx, owner, option); err != nil {
		return fmt.Errorf("创建用户仓库 %s: %w", fullName, err)
	}
	return nil
}

func (a *giteaAdmin) AddCollaborator(ctx context.Context, fullName, user, permission string) error {
	if a.sdk == nil {
		return fmt.Errorf("缺少管理员令牌，无法添加协作者")
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	mode := gitea.AccessMode(permission)
	if _, err := a.sdk.Repositories.AddCollaborator(ctx, owner, name, user, gitea.AddCollaboratorOption{
		Permission: &mode,
	}); err != nil {
		return fmt.Errorf("添加协作者 %s 到 %s: %w", user, fullName, err)
	}
	return nil
}

// EnsureBranchProtection 写入统一的分支保护策略：
//   - required approvals（内容批准 + 状态会签）；
//   - 合并白名单只含 merger：只有它可以把 PR 合入（自动化会签即合并）；
//   - 驳回阻塞、过期批准作废、落后分支阻塞；
//   - 管理员须遵守分支保护规则（AllowAdminOverride=false 时勾选），防止
//     身为管理员的 merger 绕过审批/检查；
//   - block_on_official_review_requests 勾选：存在 pending 的官方评审请求时
//     阻止合并。sync 会把 /review、@提及 登记为正式评审请求；内容评审者提交
//     review 时 Gitea 删除其请求行、门禁自动解除（API 的 requested_reviewers
//     字段有显示滞后，不代表门禁状态；sync 的撤回调用是版本兼容兜底）。团队
//     评审请求不会因成员 review 自动清除，需人工移除后才会解除。
func (a *giteaAdmin) EnsureBranchProtection(
	ctx context.Context,
	fullName string,
	options ProtectionOptions,
) error {
	if a.sdk == nil {
		return fmt.Errorf("缺少管理员令牌，无法配置分支保护")
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	protections, _, err := a.sdk.Repositories.ListBranchProtections(ctx, owner, name, gitea.ListBranchProtectionsOptions{})
	if err != nil {
		return fmt.Errorf("读取 %s 分支保护: %w", fullName, err)
	}
	enabled := true
	blockAdmin := !options.AllowAdminOverride
	whitelist := []string{options.MergerName}
	for _, protection := range protections {
		if protection.RuleName != options.Branch {
			continue
		}
		required := options.RequiredApprovals
		if _, _, err := a.sdk.Repositories.EditBranchProtection(ctx, owner, name, protection.RuleName, gitea.EditBranchProtectionOption{
			RequiredApprovals:             &required,
			BlockOnRejectedReviews:        &enabled,
			BlockOnOfficialReviewRequests: &enabled,
			DismissStaleApprovals:         &enabled,
			BlockOnOutdatedBranch:         &enabled,
			EnableMergeWhitelist:          &enabled,
			MergeWhitelistUsernames:       whitelist,
			BlockAdminMergeOverride:       &blockAdmin,
		}); err != nil {
			return fmt.Errorf("更新 %s 分支保护: %w", fullName, err)
		}
		return nil
	}
	if _, _, err := a.sdk.Repositories.CreateBranchProtection(ctx, owner, name, gitea.CreateBranchProtectionOption{
		BranchName:                    options.Branch,
		RequiredApprovals:             options.RequiredApprovals,
		BlockOnRejectedReviews:        true,
		BlockOnOfficialReviewRequests: true,
		DismissStaleApprovals:         true,
		BlockOnOutdatedBranch:         true,
		EnableMergeWhitelist:          true,
		MergeWhitelistUsernames:       whitelist,
		BlockAdminMergeOverride:       blockAdmin,
	}); err != nil {
		return fmt.Errorf("创建 %s 分支保护: %w", fullName, err)
	}
	return nil
}

func (a *giteaAdmin) ReconcileLabels(ctx context.Context, fullName, reviewerToken string) error {
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	client, err := status.NewClient(a.host, reviewerToken)
	if err != nil {
		return err
	}
	manager := status.NewManager(client, status.WithProgress(a.log))
	return manager.ReconcileLabels(ctx, status.Repository{Owner: owner, Name: name})
}

type requestAuth struct {
	token    string
	user     string
	password string
}

func (a *giteaAdmin) auth() requestAuth {
	return requestAuth{token: a.token}
}

func (a *giteaAdmin) do(
	ctx context.Context,
	method, path string,
	auth requestAuth,
	body, out any,
) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.host+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	switch {
	case auth.token != "":
		request.Header.Set("Authorization", "token "+auth.token)
	case auth.user != "":
		request.SetBasicAuth(auth.user, auth.password)
	}
	response, err := a.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response, err
	}
	if response.StatusCode/100 != 2 {
		return response, &httpError{
			Method:  method,
			Path:    path,
			Status:  response.StatusCode,
			Message: apiMessage(data),
		}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return response, fmt.Errorf("解析 %s %s 响应: %w", method, path, err)
		}
	}
	return response, nil
}

type httpError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *httpError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

func isHTTPStatus(err error, status int) bool {
	var httpErr *httpError
	return errors.As(err, &httpErr) && httpErr.Status == status
}

func apiMessage(data []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(payload.Message)
}
