package setup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultOAuthClientID 是 Gitea 内置的 `tea` 公共客户端（PKCE，无密钥）。
	// 内置客户端在旧版 Gitea 上是 confidential（不可用），届时用
	// --oauth-client-id 传入自建的公共 OAuth2 应用。
	DefaultOAuthClientID = "d57cb8c4-630c-4168-8324-ec79935e18d4"
	// oauthTimeout 是等待用户在浏览器完成授权的时长。
	oauthTimeout = 10 * time.Minute
)

// OAuthOptions 是一次 OAuth2 授权码 + PKCE 登录的参数。
type OAuthOptions struct {
	Host         string
	ClientID     string
	ClientSecret string
	// Port 是本地回调端口；0 表示随机空闲端口。
	Port int
	// OpenBrowser 打开授权页；nil 时回退系统默认浏览器（失败仅提示 URL）。
	OpenBrowser func(url string) error
	Log         func(format string, arguments ...any)
	HTTPClient  *http.Client
	Timeout     time.Duration
}

// OAuthResult 是 OAuth 登录结果。access token 短期有效；refresh token 可
// 持久化，用于运行期换取新的 access token。
type OAuthResult struct {
	Token        string
	RefreshToken string
	Login        string
	IsAdmin      bool
}

type oauthCallback struct {
	code string
	err  error
}

// OAuthLogin 走 OAuth2 授权码 + PKCE：本地监听回调、打开浏览器、兑换令牌并
// 校验身份。令牌用于 setup 期间的 API 调用（不写盘——OAuth 令牌会过期）。
func OAuthLogin(ctx context.Context, options OAuthOptions) (OAuthResult, error) {
	if options.ClientID == "" {
		options.ClientID = DefaultOAuthClientID
	}
	if options.Port < 0 || options.Port > 65535 {
		return OAuthResult{}, fmt.Errorf("oauth 回调端口非法：%d", options.Port)
	}
	if options.Timeout == 0 {
		options.Timeout = oauthTimeout
	}
	logf := options.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	host := strings.TrimRight(strings.TrimSpace(options.Host), "/")

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", options.Port))
	if err != nil {
		return OAuthResult{}, fmt.Errorf("监听本地回调端口失败（可换 --oauth-port）：%w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d", port)

	verifier, challenge, err := newPKCE()
	if err != nil {
		listener.Close()
		return OAuthResult{}, err
	}
	state, err := randomToken(16)
	if err != nil {
		listener.Close()
		return OAuthResult{}, err
	}

	callbacks := make(chan oauthCallback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("state") != state {
			callbacks <- oauthCallback{err: fmt.Errorf("OAuth 回调 state 不匹配")}
			http.Error(writer, "state mismatch", http.StatusBadRequest)
			return
		}
		if message := query.Get("error_description"); message != "" {
			callbacks <- oauthCallback{err: fmt.Errorf("OAuth 授权失败：%s", message)}
			http.Error(writer, message, http.StatusBadRequest)
			return
		}
		code := query.Get("code")
		if code == "" {
			callbacks <- oauthCallback{err: fmt.Errorf("OAuth 回调缺少 code")}
			http.Error(writer, "missing code", http.StatusBadRequest)
			return
		}
		callbacks <- oauthCallback{code: code}
		_, _ = io.WriteString(writer, "<html><body>授权完成，可以关闭此页面。</body></html>")
	})
	server := &http.Server{Handler: mux}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	authorizeURL := host + "/login/oauth/authorize?" + url.Values{
		"client_id":             {options.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	logf("OAuth 授权：请在浏览器完成登录与授权")
	open := options.OpenBrowser
	if open == nil {
		open = openBrowser
	}
	if err := open(authorizeURL); err != nil {
		logf("无法自动打开浏览器：%v", err)
		logf("请手动访问：%s", authorizeURL)
	} else {
		logf("若浏览器未打开，请手动访问：%s", authorizeURL)
	}

	timer := time.NewTimer(options.Timeout)
	defer timer.Stop()
	var callback oauthCallback
	select {
	case callback = <-callbacks:
	case <-ctx.Done():
		return OAuthResult{}, ctx.Err()
	case <-timer.C:
		return OAuthResult{}, fmt.Errorf(
			"等待 OAuth 授权超时（%s）。若浏览器显示「Unregistered Redirect URI」，"+
				"请在 Gitea「设置 → 应用」创建公共 OAuth2 应用（重定向 URI：http://127.0.0.1）"+
				"并用 --oauth-client-id 指定其 Client ID", options.Timeout)
	}
	if callback.err != nil {
		return OAuthResult{}, callback.err
	}

	token, refreshToken, err := exchangeOAuthCode(ctx, httpClient, host, options, redirectURI, callback.code, verifier)
	if err != nil {
		return OAuthResult{}, err
	}
	login, isAdmin, err := oauthUserInfo(ctx, httpClient, host, token)
	if err != nil {
		return OAuthResult{}, err
	}
	logf("OAuth 登录成功：@%s", login)
	return OAuthResult{Token: token, RefreshToken: refreshToken, Login: login, IsAdmin: isAdmin}, nil
}

func exchangeOAuthCode(
	ctx context.Context,
	httpClient *http.Client,
	host string,
	options OAuthOptions,
	redirectURI, code, verifier string,
) (accessToken, refreshToken string, err error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {options.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	if options.ClientSecret != "" {
		form.Set("client_secret", options.ClientSecret)
	}
	payload, err := requestOAuthToken(ctx, httpClient, host, form)
	if err != nil {
		return "", "", err
	}
	return payload.AccessToken, payload.RefreshToken, nil
}

// RefreshOAuthToken 用 refresh token 换取新的 access token（运行期用于读取
// 分支保护；refresh token 若被轮换则同时返回新值）。
func RefreshOAuthToken(
	ctx context.Context,
	host, clientID, clientSecret, refreshToken string,
	httpClient *http.Client,
) (accessToken, newRefreshToken string, err error) {
	if clientID == "" || refreshToken == "" {
		return "", "", fmt.Errorf("缺少 OAuth client_id 或 refresh_token")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	payload, err := requestOAuthToken(ctx, httpClient, strings.TrimRight(host, "/"), form)
	if err != nil {
		return "", "", err
	}
	if payload.RefreshToken == "" {
		payload.RefreshToken = refreshToken
	}
	return payload.AccessToken, payload.RefreshToken, nil
}

type oauthTokenPayload struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func requestOAuthToken(
	ctx context.Context,
	httpClient *http.Client,
	host string,
	form url.Values,
) (oauthTokenPayload, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, host+"/login/oauth/access_token", strings.NewReader(form.Encode()),
	)
	if err != nil {
		return oauthTokenPayload{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return oauthTokenPayload{}, fmt.Errorf("请求 OAuth 令牌：%w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload oauthTokenPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return oauthTokenPayload{}, fmt.Errorf("解析 OAuth 令牌响应：%w", err)
	}
	if payload.AccessToken == "" {
		message := payload.ErrorDescription
		if message == "" {
			message = payload.Error
		}
		if message == "" {
			message = strings.TrimSpace(string(body))
		}
		return oauthTokenPayload{}, fmt.Errorf("OAuth 令牌请求失败（HTTP %d）：%s", response.StatusCode, message)
	}
	return payload, nil
}

func oauthUserInfo(ctx context.Context, httpClient *http.Client, host, token string) (string, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/v1/user", nil)
	if err != nil {
		return "", false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := httpClient.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("校验 OAuth 令牌：%w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode/100 != 2 {
		return "", false, fmt.Errorf("OAuth 令牌不可用（HTTP %d）：%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Login   string `json:"login"`
		IsAdmin bool   `json:"is_admin"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Login == "" {
		return "", false, fmt.Errorf("Gitea 返回缺 login")
	}
	return payload.Login, payload.IsAdmin, nil
}

func newPKCE() (verifier, challenge string, err error) {
	raw, err := randomToken(32)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte(raw))
	return raw, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// openBrowser 尽力打开系统浏览器；失败不影响流程（URL 已打印）。
func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}
