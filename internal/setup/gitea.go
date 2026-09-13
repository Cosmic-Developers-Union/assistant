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

	"assistant/internal/instances"
	"assistant/internal/status"
)

const (
	adminTokenName = "assistant-admin"
	// 机器人令牌的最小权限集：仓库读写（分支/协作者/合并）、Issue 读写
	// （标签、评论、PR review）与读取自身账号（完成判定的身份校验）。
	botTokenScopes = "read:repository,write:repository,read:issue,write:issue,read:user"
	httpTimeout    = 30 * time.Second
)

// giteaAdmin 是 Admin 的真实实现：读操作走 raw REST，写操作走 Gitea SDK。
type giteaAdmin struct {
	host          string
	token         string
	basicUser     string
	basicPassword string
	http          *http.Client
	sdk           *gitea.Client
	dryRun        bool
	// ephemeral 表示令牌来自 OAuth 登录（会过期），不应写入配置。
	ephemeral bool
	// oauth 是 OAuth 登录留下的刷新凭据（可落盘，运行期换取 access token）。
	oauth *instances.OAuthCredential
	log   func(string, ...any)
	// passwords 记录本次创建的机器人账号随机密码，用于令牌创建失败时以
	// Basic Auth 回退（不写盘）。
	passwords map[string]string
}

// NewAdmin 构造高权限操作面：校验管理员身份。凭据来源：
//   - --oauth：OAuth2 授权码 + PKCE 浏览器登录（令牌不落盘）；
//   - --admin-token / 已有配置：直接校验；
//   - --admin-user/--admin-password：用它换取一个长期管理员令牌。
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
		host:          options.Host,
		token:         options.AdminToken,
		basicUser:     options.AdminUser,
		basicPassword: options.AdminPassword,
		http:          httpClient,
		dryRun:        options.DryRun,
		log:           logf,
		passwords:     map[string]string{},
	}
	var login string
	var isAdmin bool
	if options.OAuth != nil {
		oauthOptions := *options.OAuth
		oauthOptions.Host = options.Host
		if oauthOptions.ClientID == "" {
			oauthOptions.ClientID = DefaultOAuthClientID
		}
		if oauthOptions.Log == nil {
			oauthOptions.Log = logf
		}
		result, err := OAuthLogin(ctx, oauthOptions)
		if err != nil {
			return nil, err
		}
		admin.token = result.Token
		admin.ephemeral = true
		admin.oauth = &instances.OAuthCredential{
			ClientID:     oauthOptions.ClientID,
			ClientSecret: oauthOptions.ClientSecret,
			RefreshToken: result.RefreshToken,
		}
		login, isAdmin = result.Login, result.IsAdmin
	} else {
		if admin.token == "" && !admin.dryRun {
			token, err := admin.createTokenWithBasic(ctx, options.AdminUser, options.AdminPassword, adminTokenName)
			if err != nil {
				return nil, fmt.Errorf("用管理员账号生成令牌: %w", err)
			}
			admin.token = token
			logf("已为管理员账号 @%s 生成访问令牌", options.AdminUser)
		}
		var err error
		login, isAdmin, err = admin.AuthenticatedUser(ctx)
		if err != nil {
			return nil, fmt.Errorf("校验管理员凭据: %w", err)
		}
	}
	if !isAdmin {
		return nil, fmt.Errorf("账号 @%s 不是管理员，setup 需要管理员权限", login)
	}
	if admin.token != "" {
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
	}
	return admin, nil
}

func (a *giteaAdmin) AdminToken() string {
	return a.token
}

// PersistentToken 返回可长期使用的管理员令牌；OAuth 令牌会过期，返回空。
func (a *giteaAdmin) PersistentToken() string {
	if a.ephemeral {
		return ""
	}
	return a.token
}

// AdminOAuth 返回 OAuth 刷新凭据（非 OAuth 登录时为 nil）。
func (a *giteaAdmin) AdminOAuth() *instances.OAuthCredential {
	if a.oauth == nil {
		return nil
	}
	copied := *a.oauth
	return &copied
}

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

// ConvergeToken 把账号令牌收敛为唯一一个：保留 keepToken（按末 8 位匹配）
// 或新建 tokenName，删除账号下其余所有令牌。reviewer 用：同一站点同时只允许
// 一个评审主机（dispatcher 单飞锁是进程内的），令牌唯一从凭据层面强制约束。
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
		if item.ID == keepID {
			continue
		}
		if err := a.deleteTokenBasic(ctx, name, password, item.ID); err != nil {
			return "", false, fmt.Errorf("删除 %s 的历史令牌 %s: %w", name, item.Name, err)
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
		a.log("已清理 %s 的 %d 个历史令牌（保证单一评审主机）", name, deleted)
	}
	return token, created, nil
}

// EnsureRepoToken 保证 tokenName 令牌存在（保留 keepToken 或新建），只清理
// 同名旧令牌与历史共享名（assistant / assistant-setup-*），不影响账号下其他
// 仓库的独立令牌。merger 用：每个项目一个自己的令牌。
func (a *giteaAdmin) EnsureRepoToken(
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
		if item.ID == keepID {
			continue
		}
		if item.Name == tokenName || isLegacySharedTokenName(item.Name) {
			if err := a.deleteTokenBasic(ctx, name, password, item.ID); err != nil {
				return "", false, fmt.Errorf("删除 %s 的令牌 %s: %w", name, item.Name, err)
			}
			deleted++
		}
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
		a.log("已清理 %s 的 %d 个同名/历史令牌（%s）", name, deleted, tokenName)
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

// isLegacySharedTokenName 是旧版共用令牌的命名：merger 收敛时会清掉这些，
// 避免多个项目复用同一个令牌。
func isLegacySharedTokenName(name string) bool {
	return name == "assistant" || strings.HasPrefix(name, "assistant-setup-")
}

// createTokenBasic 以机器人自己的 Basic Auth 建一个指定名称的令牌。
// Gitea 的建令牌端点只接受 Basic Auth：管理员令牌（含 OAuth 令牌）会 401/403。
func (a *giteaAdmin) createTokenBasic(ctx context.Context, name, password, tokenName string) (string, error) {
	body := map[string]any{"name": tokenName, "scopes": strings.Split(botTokenScopes, ",")}
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

// SetRepoVariable 写仓库级 Actions variable：不存在时创建（POST）、存在时
// 更新（PUT）——Gitea 的 PUT 语义是仅更新，对不存在的 variable 返回 404。
func (a *giteaAdmin) SetRepoVariable(ctx context.Context, fullName, name, value string) error {
	owner, repository, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/variables/%s",
		url.PathEscape(owner), url.PathEscape(repository), url.PathEscape(name))
	method := http.MethodPut
	if _, getErr := a.do(ctx, http.MethodGet, path, a.auth(), nil, nil); getErr != nil {
		if !isHTTPStatus(getErr, http.StatusNotFound) {
			return fmt.Errorf("读 %s 的 Actions variable %s: %w", fullName, name, getErr)
		}
		method = http.MethodPost
	}
	if _, err := a.do(ctx, method, path, a.auth(), map[string]any{"value": value}, nil); err != nil {
		return fmt.Errorf("写 %s 的 Actions variable %s: %w", fullName, name, err)
	}
	return nil
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
//   - 明确关闭 block_on_official_review_requests：它按 pending 的 official
//     review request 阻止合并，而 Gitea 提交 review 后不消费
//     requested_reviewers，勾选会把自动合并永久卡死。
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
	disabled := false
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
			BlockOnOfficialReviewRequests: &disabled,
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
		BlockOnOfficialReviewRequests: false,
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
	return requestAuth{token: a.token, user: a.basicUser, password: a.basicPassword}
}

func (a *giteaAdmin) createTokenWithBasic(ctx context.Context, user, password, tokenName string) (string, error) {
	var payload struct {
		Token string `json:"sha1"`
	}
	body := map[string]any{"name": tokenName, "scopes": []string{"all"}}
	path := "/api/v1/users/" + url.PathEscape(user) + "/tokens"
	if _, err := a.do(ctx, http.MethodPost, path, requestAuth{user: user, password: password}, body, &payload); err != nil {
		return "", err
	}
	if payload.Token == "" {
		return "", fmt.Errorf("Gitea 未返回令牌")
	}
	return payload.Token, nil
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
