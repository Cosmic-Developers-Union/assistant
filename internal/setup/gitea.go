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
	log           func(string, ...any)
	// passwords 记录本次创建的机器人账号随机密码，用于令牌创建失败时以
	// Basic Auth 回退（不写盘）。
	passwords map[string]string
}

// NewAdmin 构造高权限操作面：校验管理员身份；未提供 --admin-token 时，用
// --admin-user/--admin-password 生成一个管理员令牌（dry-run 不生成）。
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
	if admin.token == "" && !admin.dryRun {
		token, err := admin.createTokenWithBasic(ctx, options.AdminUser, options.AdminPassword, adminTokenName)
		if err != nil {
			return nil, fmt.Errorf("用管理员账号生成令牌: %w", err)
		}
		admin.token = token
		logf("已为管理员账号 @%s 生成访问令牌", options.AdminUser)
	}
	login, isAdmin, err := admin.AuthenticatedUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("校验管理员凭据: %w", err)
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

func (a *giteaAdmin) CreateToken(ctx context.Context, name string) (string, error) {
	tokenName := fmt.Sprintf("assistant-setup-%d", time.Now().Unix())
	body := map[string]any{"name": tokenName, "scopes": strings.Split(botTokenScopes, ",")}
	var payload struct {
		Token string `json:"sha1"`
	}
	path := "/api/v1/users/" + url.PathEscape(name) + "/tokens"
	_, err := a.do(ctx, http.MethodPost, path, a.auth(), body, &payload)
	// 部分 Gitea 版本不允许管理员令牌代建他人令牌，回退用机器人自己的
	// Basic Auth（仅当本次创建了该账号、密码在手）。
	if err != nil && (isHTTPStatus(err, http.StatusForbidden) || isHTTPStatus(err, http.StatusUnauthorized)) {
		if password := a.passwords[name]; password != "" {
			_, err = a.do(ctx, http.MethodPost, path, requestAuth{user: name, password: password}, body, &payload)
		}
	}
	if err != nil {
		return "", fmt.Errorf("为 %s 创建令牌: %w", name, err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("为 %s 创建令牌: Gitea 未返回令牌", name)
	}
	return payload.Token, nil
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

func (a *giteaAdmin) AddCollaborator(ctx context.Context, fullName, user string) error {
	if a.sdk == nil {
		return fmt.Errorf("缺少管理员令牌，无法添加协作者")
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return err
	}
	permission := gitea.AccessModeWrite
	if _, err := a.sdk.Repositories.AddCollaborator(ctx, owner, name, user, gitea.AddCollaboratorOption{
		Permission: &permission,
	}); err != nil {
		return fmt.Errorf("添加协作者 %s 到 %s: %w", user, fullName, err)
	}
	return nil
}

func (a *giteaAdmin) EnsureBranchProtection(ctx context.Context, fullName, branch string, requiredApprovals int64) error {
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
	for _, protection := range protections {
		if protection.RuleName != branch {
			continue
		}
		required := requiredApprovals
		if _, _, err := a.sdk.Repositories.EditBranchProtection(ctx, owner, name, protection.RuleName, gitea.EditBranchProtectionOption{
			RequiredApprovals:      &required,
			BlockOnRejectedReviews: &enabled,
			DismissStaleApprovals:  &enabled,
			BlockOnOutdatedBranch:  &enabled,
		}); err != nil {
			return fmt.Errorf("更新 %s 分支保护: %w", fullName, err)
		}
		return nil
	}
	if _, _, err := a.sdk.Repositories.CreateBranchProtection(ctx, owner, name, gitea.CreateBranchProtectionOption{
		BranchName:             branch,
		RequiredApprovals:      requiredApprovals,
		BlockOnRejectedReviews: true,
		DismissStaleApprovals:  true,
		BlockOnOutdatedBranch:  true,
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
