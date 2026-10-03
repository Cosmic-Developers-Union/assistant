// Package project 提供站点机器人接入与当前项目的规范化；不参与常驻调度。
package project

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"uuid"

	gitea "gitea.dev/sdk"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// Client 只使用官方 SDK，身份来自显式选择的个人实例。
type Client struct {
	SDK   *gitea.Client
	Entry credentials.Gitea
}

// NewClient 限制请求时长并禁止重定向带走认证信息。
func NewClient(entry credentials.Gitea) (*Client, error) {
	if err := credentials.ValidateHost(entry.URL); err != nil {
		return nil, err
	}
	sdk, err := gitea.NewClient(entry.URL, gitea.SetToken(entry.Token), gitea.SetGiteaVersion(""), gitea.SetHTTPClient(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	if err != nil {
		return nil, fmt.Errorf("创建 Gitea 接入: %w", err)
	}
	return &Client{sdk, entry}, nil
}

var accountName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ProvisionAccount 创建缺失账号并发令牌；已有令牌复用，不重置现有账号密码。
// 每个返回值应立即保存，避免第二个账号失败丢失第一个账号的凭据。
func (c *Client) ProvisionAccount(ctx context.Context, name, user, email, password string, existing *credentials.Gitea, dry bool) (credentials.Gitea, error) {
	if !accountName.MatchString(user) || name == "" || email == "" {
		return credentials.Gitea{}, fmt.Errorf("机器人名称、用户名或邮箱无效")
	}
	me, _, err := c.SDK.Users.GetMyUserInfo(ctx)
	if err != nil {
		return credentials.Gitea{}, fmt.Errorf("校验管理员: %w", err)
	}
	if !me.IsAdmin {
		return credentials.Gitea{}, fmt.Errorf("实例账号不是 Gitea 管理员")
	}
	if existing != nil {
		if credentials.NormalizeHost(existing.URL) != credentials.NormalizeHost(c.Entry.URL) || existing.Username != user {
			return credentials.Gitea{}, fmt.Errorf("实例名已被不同站点或账号使用")
		}
		old, err := NewClient(*existing)
		if err != nil {
			return credentials.Gitea{}, err
		}
		identity, _, err := old.SDK.Users.GetMyUserInfo(ctx)
		if err == nil && identity.UserName == user {
			return *existing, nil
		}
		if password == "" && existing.EncryptedPassword != "" {
			password, err = existing.PasswordValue()
			if err != nil {
				return credentials.Gitea{}, err
			}
		}
		if password == "" && c.Entry.EncryptedPassword == "" {
			return credentials.Gitea{}, fmt.Errorf("已存账号令牌失效；提供该账号的 password-file 重新发令牌，不自动重置密码")
		}
	}
	_, response, err := c.SDK.Users.GetUserInfo(ctx, user)
	missing := response != nil && response.StatusCode == http.StatusNotFound
	if err != nil && !missing {
		return credentials.Gitea{}, fmt.Errorf("查询机器人账号: %w", err)
	}
	entry := credentials.Gitea{Name: name, URL: c.Entry.URL, Username: user}
	if existing != nil {
		entry.EncryptedPassword = existing.EncryptedPassword
	}
	if dry {
		return entry, nil
	}
	if missing {
		if password == "" {
			password = rand.Text() + "aA1!"
		}
		if _, _, err := c.SDK.Admin.CreateUser(ctx, gitea.CreateUserOption{Username: user, Email: email, Password: password, MustChangePassword: new(false)}); err != nil {
			return entry, fmt.Errorf("创建机器人账号: %w", credentials.RedactError(err, password))
		}
	} else if password == "" && c.Entry.EncryptedPassword == "" {
		return entry, fmt.Errorf("账号 @%s 已存在但无本地有效凭据；用 instance add gitea 或提供该账号 password-file 登记", user)
	}
	var token string
	if password == "" {
		adminPassword, passwordErr := c.Entry.PasswordValue()
		if passwordErr != nil {
			return entry, passwordErr
		}
		token, err = IssueTokenForUser(ctx, c.Entry, user, adminPassword, "")
	} else {
		token, err = IssueToken(ctx, c.Entry.URL, user, password)
	}
	if err != nil {
		return entry, err
	}
	entry.Token = token
	if password != "" {
		if err := entry.SetPassword(password); err != nil {
			return entry, err
		}
	}
	return entry, nil
}

// IssueTokenForUser 用已登录账号的密码认证发令牌；跨账号操作必须由真实管理员执行。
// 目标账号与认证账号分开，不能把管理员密码或令牌保存到目标账号名下。
func IssueTokenForUser(ctx context.Context, actor credentials.Gitea, target, password, otp string) (result string, resultErr error) {
	defer func() { resultErr = credentials.RedactError(resultErr, password, actor.Token) }()
	if !accountName.MatchString(target) || password == "" {
		return "", fmt.Errorf("目标用户名或登录密码无效")
	}
	if err := credentials.ValidateHost(actor.URL); err != nil {
		return "", err
	}
	client, err := gitea.NewClient(actor.URL, gitea.SetBasicAuth(actor.Username, password), gitea.SetOTP(otp), gitea.SetGiteaVersion(""), gitea.SetHTTPClient(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	if err != nil {
		return "", fmt.Errorf("创建令牌认证: %w", err)
	}
	me, _, err := client.Users.GetMyUserInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("校验登录账号: %w", err)
	}
	if !strings.EqualFold(me.UserName, actor.Username) {
		return "", fmt.Errorf("平台登录身份不匹配")
	}
	admin := me.IsAdmin
	if !strings.EqualFold(target, me.UserName) {
		if !admin {
			return "", fmt.Errorf("为其他用户创建令牌需要 Gitea 管理员身份")
		}
		user, _, err := client.Users.GetUserInfo(ctx, target)
		if err != nil {
			return "", fmt.Errorf("查询目标用户: %w", err)
		}
		if !strings.EqualFold(user.UserName, target) {
			return "", fmt.Errorf("目标用户身份不匹配")
		}
		admin = user.IsAdmin
	}
	// SDK 将令牌所属用户名与 BasicAuth 用户名绑定。保留其请求与序列化实现，
	// 仅在传输层恢复认证账号，让目标路径与实际登录身份分别表达。
	client, err = gitea.NewClient(actor.URL, gitea.SetBasicAuth(target, password), gitea.SetOTP(otp), gitea.SetGiteaVersion(""), gitea.SetHTTPClient(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: tokenAuthentication{actor.Username, password}}))
	if err != nil {
		return "", fmt.Errorf("创建目标令牌客户端: %w", err)
	}
	token, _, err := client.Users.CreateAccessToken(ctx, gitea.CreateAccessTokenOption{Name: "assistant-bot-" + uuid.New().String(), Scopes: TokenScopes(admin)})
	if err != nil {
		return "", fmt.Errorf("为 @%s 创建令牌: %w", target, err)
	}
	if token.Token == "" {
		return "", fmt.Errorf("平台没有返回新令牌")
	}
	return token.Token, nil
}

type tokenAuthentication struct{ user, password string }

func (a tokenAuthentication) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.SetBasicAuth(a.user, a.password)
	return http.DefaultTransport.RoundTrip(request)
}

// IssueToken 只新建自己的工具令牌，不删除账号的其他令牌，密码不持久化。
func IssueToken(ctx context.Context, host, user, password string) (string, error) {
	if err := credentials.ValidateHost(host); err != nil {
		return "", err
	}
	sdk, err := gitea.NewClient(host, gitea.SetBasicAuth(user, password), gitea.SetGiteaVersion(""), gitea.SetHTTPClient(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	if err != nil {
		return "", err
	}
	me, _, err := sdk.Users.GetMyUserInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("校验令牌账号: %w", credentials.RedactError(err, password))
	}
	if me.UserName != user {
		return "", fmt.Errorf("平台登录身份不匹配")
	}
	token, _, err := sdk.Users.CreateAccessToken(ctx, gitea.CreateAccessTokenOption{Name: "assistant-bot-" + uuid.New().String(), Scopes: TokenScopes(me.IsAdmin)})
	if err != nil {
		return "", fmt.Errorf("创建机器人令牌: %w", credentials.RedactError(err, password))
	}
	if token.Token == "" {
		return "", fmt.Errorf("平台没有返回新令牌")
	}
	return token.Token, nil
}

// TokenScopes 让管理员实例能够执行站点接入，机器人仍只有仓库与 Issue 权限。
func TokenScopes(admin bool) []gitea.AccessTokenScope {
	scopes := []gitea.AccessTokenScope{"read:user", "read:organization", "write:repository", "write:issue"}
	if admin {
		scopes = append(scopes, "write:admin")
	}
	return scopes
}
