//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"assistant/internal/setup"
	"assistant/internal/status"
)

var (
	inputTagPattern   = regexp.MustCompile(`<input[^>]*type="hidden"[^>]*>`)
	inputNamePattern  = regexp.MustCompile(`name="([^"]+)"`)
	inputValuePattern = regexp.MustCompile(`value="([^"]*)"`)
	formActionPattern = regexp.MustCompile(`<form[^>]*action="([^"]*)"`)
)

// hiddenInputs 提取表单里的隐藏字段（属性顺序不敏感）。
func hiddenInputs(html string) url.Values {
	values := url.Values{}
	for _, tag := range inputTagPattern.FindAllString(html, -1) {
		name := inputNamePattern.FindStringSubmatch(tag)
		if name == nil {
			continue
		}
		value := ""
		if match := inputValuePattern.FindStringSubmatch(tag); match != nil {
			value = match[1]
		}
		values.Set(name[1], value)
	}
	return values
}

// TestSetupWithOAuthEndToEnd 验证 OAuth2 授权码 + PKCE 登录可以替代 admin_token：
// 用自动化浏览器（登录表单 + 授权确认）完成真实 Gitea 的 OAuth 流程，setup 用
// 短期 OAuth 令牌完成初始化，且不把令牌写进配置。
func TestSetupWithOAuthEndToEnd(t *testing.T) {
	env := e2eEnvironmentFromFile(t)
	ctx := context.Background()

	adminClient, err := status.NewClient(env.Host, env.AdminToken)
	if err != nil {
		t.Fatal(err)
	}
	adminLogin, err := adminClient.AuthenticatedUser(ctx)
	if err != nil {
		t.Fatalf("管理员令牌不可用: %v", err)
	}

	// Gitea 1.22 的公共客户端按精确 URI 匹配重定向，用固定空闲端口；
	// 新版 Gitea 忽略 loopback 端口，同样兼容。
	port := freePort(t)
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d", port)
	clientID := createOAuthApplication(t, env.Host, env.AdminToken, redirectURI)

	repositoryName := fmt.Sprintf("e2e-oauth-%d", time.Now().Unix())
	fullName := adminLogin + "/" + repositoryName

	options := setup.Options{
		Host: env.Host,
		OAuth: &setup.OAuthOptions{
			Host:     env.Host,
			ClientID: clientID,
			Port:     port,
			Timeout:  30 * time.Second,
			Log:      t.Logf,
			OpenBrowser: func(authorizeURL string) error {
				return browserAuthorize(t, env, authorizeURL, redirectURI)
			},
		},
		Repos:       []string{fullName},
		CreateRepos: true,
		Log:         t.Logf,
	}
	admin, err := setup.NewAdmin(ctx, options)
	if err != nil {
		t.Fatalf("NewAdmin(oauth) error = %v", err)
	}
	if token := admin.PersistentToken(); token != "" {
		t.Errorf("OAuth token must not be persisted, got %d chars", len(token))
	}
	instance, err := setup.Run(ctx, options, admin)
	if err != nil {
		t.Fatalf("Run(oauth) error = %v", err)
	}
	if instance.AdminToken != "" {
		t.Errorf("AdminToken = %q, want empty (OAuth 令牌不落盘)", instance.AdminToken)
	}
	if instance.AdminOAuth == nil || instance.AdminOAuth.RefreshToken == "" || instance.AdminOAuth.ClientID != clientID {
		t.Fatalf("AdminOAuth = %+v, want persisted refresh credential", instance.AdminOAuth)
	}
	if instance.Reviewer.Token == "" || len(instance.Repos) != 1 || instance.Repos[0].MergerToken == "" {
		t.Fatalf("bot tokens missing: %+v", instance)
	}

	// 机器人令牌由 OAuth 管理员通过「重置密码 + BasicAuth」链路创建，仍然可用
	for _, account := range []struct{ name, token string }{
		{instance.Reviewer.Name, instance.Reviewer.Token},
		{instance.Merger.Name, instance.Repos[0].MergerToken},
	} {
		client, err := status.NewClient(env.Host, account.token)
		if err != nil {
			t.Fatal(err)
		}
		login, err := client.AuthenticatedUser(ctx)
		if err != nil {
			t.Fatalf("AuthenticatedUser(%s) error = %v", account.name, err)
		}
		if login != account.name {
			t.Errorf("token identity = %q, want %q", login, account.name)
		}
	}

	// 分支保护同样配置完成；运行期不需要 admin_token——用 refresh token 换取
	// 短期 access token 读取（对应 check/sync/automerge 的实际路径）。
	accessToken, newRefresh, err := setup.RefreshOAuthToken(
		ctx, env.Host, instance.AdminOAuth.ClientID, instance.AdminOAuth.ClientSecret,
		instance.AdminOAuth.RefreshToken, nil,
	)
	if err != nil {
		t.Fatalf("RefreshOAuthToken() error = %v", err)
	}
	if accessToken == "" || newRefresh == "" {
		t.Fatalf("refresh returned empty tokens: %q %q", accessToken, newRefresh)
	}
	reviewerClient, err := status.NewClient(env.Host, instance.Reviewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewerClient.UseBranchProtectionToken(accessToken); err != nil {
		t.Fatal(err)
	}
	protections, err := reviewerClient.ListBranchProtections(ctx, status.Repository{Owner: adminLogin, Name: repositoryName})
	if err != nil {
		t.Fatalf("ListBranchProtections() error = %v", err)
	}
	found := false
	for _, protection := range protections {
		if protection.RuleName == "main" && protection.RequiredApprovals == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("protections = %+v, want main with 2 approvals", protections)
	}

	// OAuth 换取的短期令牌同样能写仓库 Actions 配置（`assistant actions` 的路径）
	oauthAdmin, err := setup.NewAdmin(ctx, setup.Options{Host: env.Host, AdminToken: accessToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.ConfigureActions(ctx, oauthAdmin, instance, false, t.Logf); err != nil {
		t.Fatalf("ConfigureActions(oauth) error = %v", err)
	}
	if value := getActionVariable(t, env.Host, accessToken, adminLogin, repositoryName, setup.ActionsVariableStateReviewer); value != instance.Merger.Name {
		t.Errorf("OAuth wrote GITEA_STATE_REVIEWER = %q, want %q", value, instance.Merger.Name)
	}

	// 第二次初始化（不传 Existing）：机器人账号已存在、密码未知，管理员只有
	// OAuth 令牌（建令牌端点 401）——校验「重置机器人密码 + BasicAuth 建令牌」
	// 回退链路；同时令牌唯一性收敛应使第一次的令牌失效。
	secondRepo := fmt.Sprintf("e2e-oauth2-%d", time.Now().Unix())
	secondOptions := options
	secondOptions.Repos = []string{adminLogin + "/" + secondRepo}
	secondOptions.Existing = nil
	second, err := setup.Run(ctx, secondOptions, admin)
	if err != nil {
		t.Fatalf("Run(oauth, existing bots) error = %v", err)
	}
	if second.Reviewer.Token == "" || second.Reviewer.Token == instance.Reviewer.Token {
		t.Errorf("second run should mint a fresh reviewer token, got %q", second.Reviewer.Token)
	}
	secondClient, err := status.NewClient(env.Host, second.Reviewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	if login, err := secondClient.AuthenticatedUser(ctx); err != nil || login != second.Reviewer.Name {
		t.Errorf("fresh token login = %q, err = %v", login, err)
	}
	// 唯一性：第一次的令牌已被收敛删除
	oldClient, err := status.NewClient(env.Host, instance.Reviewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	if login, err := oldClient.AuthenticatedUser(ctx); err == nil {
		t.Errorf("stale token still valid (login %q), uniqueness convergence failed", login)
	}
}

// e2eEnvironmentFromFile 只从 .env 读取（OAuth 测试显式清掉进程环境，避免
// 依赖外部注入）。
func e2eEnvironmentFromFile(t *testing.T) e2eEnv {
	t.Helper()
	data, err := readDotEnv()
	if err != nil {
		t.Skip("缺少 test/e2e/.env（先运行 test/gitea/up.sh）")
	}
	env := e2eEnv{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ASSISTANT_E2E_HOST":
			env.Host = value
		case "ASSISTANT_E2E_ADMIN_USER":
			env.AdminUser = value
		case "ASSISTANT_E2E_ADMIN_PASSWORD":
			env.AdminPassword = value
		case "ASSISTANT_E2E_ADMIN_TOKEN":
			env.AdminToken = value
		}
	}
	if env.Host == "" || env.AdminToken == "" {
		t.Skip("test/e2e/.env 缺少 HOST/ADMIN_TOKEN（先运行 test/gitea/up.sh）")
	}
	if env.AdminUser == "" {
		env.AdminUser = "e2eadmin"
	}
	if env.AdminPassword == "" {
		env.AdminPassword = "admin-e2e-password"
	}
	return env
}

func readDotEnv() ([]byte, error) {
	return os.ReadFile(".env")
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// createOAuthApplication 用管理员令牌注册一个公共 OAuth2 应用，返回 client_id。
func createOAuthApplication(t *testing.T, host, adminToken, redirectURI string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"name":                fmt.Sprintf("assistant-e2e-%d", time.Now().Unix()),
		"redirect_uris":       []string{redirectURI},
		"confidential_client": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, host+"/api/v1/user/applications/oauth2", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("创建 OAuth 应用: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode/100 != 2 {
		t.Fatalf("创建 OAuth 应用失败（HTTP %d）：%s", response.StatusCode, body)
	}
	var created struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ClientID == "" {
		t.Fatalf("OAuth 应用响应缺少 client_id：%s", body)
	}
	return created.ClientID
}

// browserAuthorize 扮演浏览器：登录 → 打开授权页 → 提交授权确认 → 跟随回调。
func browserAuthorize(t *testing.T, env e2eEnv, authorizeURL, callbackPrefix string) error {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	loginPage, _, err := getPage(client, env.Host+"/user/login")
	if err != nil {
		return err
	}
	loginForm := hiddenInputs(loginPage)
	loginForm.Set("user_name", env.AdminUser)
	loginForm.Set("password", env.AdminPassword)
	if _, _, err := postForm(client, env.Host+"/user/login", loginForm); err != nil {
		return err
	}

	body, finalURL, err := getPage(client, authorizeURL)
	if err != nil {
		return err
	}
	// 已有授权记录时 authorize 直接回调（Gitea 跟随到本地回调并渲染完成页）
	if strings.HasPrefix(finalURL, callbackPrefix) {
		return nil
	}
	consentForm := hiddenInputs(body)
	consentForm.Set("granted", "true")
	action := "/login/oauth/grant"
	if match := formActionPattern.FindStringSubmatch(body); match != nil {
		action = match[1]
	}
	if !strings.HasPrefix(action, "http") {
		action = env.Host + action
	}
	_, _, err = postForm(client, action, consentForm)
	return err
}

func getPage(client *http.Client, target string) (string, string, error) {
	response, err := client.Get(target)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	return string(body), response.Request.URL.String(), nil
}

func postForm(client *http.Client, target string, form url.Values) (string, string, error) {
	response, err := client.PostForm(target, form)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if response.StatusCode/100 != 2 {
		return "", response.Request.URL.String(), fmt.Errorf("POST %s: HTTP %d: %s", target, response.StatusCode, body)
	}
	return string(body), response.Request.URL.String(), nil
}
