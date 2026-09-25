package main

import (
	"cmp"
	"fmt"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/setup"

	"github.com/spf13/cobra"
)

// login token 子命令组：凭据库里用途令牌的查看与轮换。list/show 只读本地凭据；
// refresh 用账号密码在站点上删除同名旧令牌再按原权限集重建（Gitea 不回读令牌
// 值，无法原地续期），新令牌写回凭据库。整组只碰凭据库，不动 config.json。
func newLoginTokenCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "token",
		Short: "用途令牌的查看与轮换（list/show/refresh）",
		Long: "用途令牌的查看与轮换。令牌按 (host, user, purpose) 唯一落在凭据库\n" +
			"（credentials.json，0600）；purpose 取值：" + strings.Join(credentials.Purposes(), "、") + "。\n\n" +
			"  assistant login token list [host]              列出凭据库中的用途令牌\n" +
			"  assistant login token show <host> [purpose]    查看一条令牌（含明文）\n" +
			"  assistant login token refresh <host> [purpose] 轮换一条令牌（需账号密码）\n\n" +
			"purpose 缺省 mcp；账号缺省站点当前登录身份，--user 可显式指定。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newLoginTokenListCommand(), newLoginTokenShowCommand(), newLoginTokenRefreshCommand())
	return command
}

// resolveTokenUser 决定操作哪个账号的令牌：--user > 站点当前登录身份 > 站点唯一
// 账号。多账号且无身份记录时不猜，点名让调用方指定。
func resolveTokenUser(store *credentials.File, host, flagUser string) (string, error) {
	if user := strings.TrimSpace(flagUser); user != "" {
		return user, nil
	}
	if identity, ok := store.IdentityFor(host); ok {
		return identity.User, nil
	}
	users := store.Users(host)
	switch len(users) {
	case 1:
		return users[0], nil
	case 0:
		return "", fmt.Errorf("凭据库里没有 %s 的任何账号：先 assistant login add %s", host, host)
	default:
		return "", fmt.Errorf("%s 有多个账号（%s），请用 --user 指定",
			host, strings.Join(prefixedUsers(users), "、"))
	}
}

// prefixedUsers 给账号名加 @（错误信息里点名用）。
func prefixedUsers(users []string) []string {
	quoted := make([]string, 0, len(users))
	for _, user := range users {
		quoted = append(quoted, "@"+user)
	}
	return quoted
}

// resolveTokenPurpose 解析 show/refresh 的 purpose 位置参数：缺省 mcp，未登记的
// purpose 立即报错（凭据库只认已登记用途）。
func resolveTokenPurpose(args []string) (string, error) {
	purpose := credentials.PurposeMCP
	if len(args) == 2 {
		purpose = strings.TrimSpace(args[1])
	}
	if !credentials.KnownPurpose(purpose) {
		return "", fmt.Errorf("未知用途 %q（支持 %s）", purpose, strings.Join(credentials.Purposes(), "、"))
	}
	return purpose, nil
}

func newLoginTokenListCommand() *cobra.Command {
	var userFilter string
	command := &cobra.Command{
		Use:   "list [host]",
		Short: "列出凭据库中的用途令牌（只显示末 8 位，不出明文）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			host := ""
			if len(args) == 1 {
				host = strings.TrimRight(strings.TrimSpace(args[0]), "/")
			}
			stdout := command.OutOrStdout()
			path, err := credentials.Path()
			if err != nil {
				return err
			}
			store, err := credentials.Load(path)
			if err != nil {
				return err
			}
			if store.Empty() {
				fmt.Fprintln(stdout, "凭据库为空（assistant login add <host> 登记）")
				return nil
			}
			user := strings.TrimSpace(userFilter)
			matched := 0
			for _, credential := range store.Credentials {
				if host != "" && !sameHost(credential.Host, host) {
					continue
				}
				if user != "" && credential.User != user {
					continue
				}
				matched++
				fmt.Fprintf(stdout, "%s\t%s@%s\t%s\t%s\t%s\t%s\n",
					credential.Host, credential.Purpose, credential.User,
					orDash(credential.TokenName), orDash(credentials.LastEight(credential.Token)),
					orDash(credential.Source), orDash(credential.CreatedAt))
			}
			if matched == 0 {
				fmt.Fprintln(stdout, "没有匹配的令牌")
			}
			fmt.Fprintf(stdout, "凭据库：%s\n", path)
			return nil
		},
	}
	command.Flags().StringVar(&userFilter, "user", "", "只列该账号的令牌")
	return command
}

// tokenShowOptions 是 show 的输入：--user 显式点名账号，--check 顺带向站点验证
// 令牌仍然有效。
type tokenShowOptions struct {
	User  string
	Check bool
}

func newLoginTokenShowCommand() *cobra.Command {
	options := &tokenShowOptions{}
	command := &cobra.Command{
		Use:   "show <host> [purpose]",
		Short: "查看一条用途令牌的记录（输出令牌明文）",
		Long: "查看凭据库里一条用途令牌：记录字段 + 令牌明文（供粘贴到 assistant 之外\n" +
			"的配置）。--check 顺带用该令牌向站点验证，确认它仍然有效；只读本地时\n" +
			"不发任何网络请求。",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			host := strings.TrimRight(strings.TrimSpace(args[0]), "/")
			purpose, err := resolveTokenPurpose(args)
			if err != nil {
				return err
			}
			return runTokenShow(command, host, purpose, options)
		},
	}
	command.Flags().StringVar(&options.User, "user", "", "令牌所属账号（缺省站点当前登录身份）")
	command.Flags().BoolVar(&options.Check, "check", false, "用该令牌向站点验证有效性（发起网络请求）")
	return command
}

func runTokenShow(command *cobra.Command, host, purpose string, options *tokenShowOptions) error {
	path, err := credentials.Path()
	if err != nil {
		return err
	}
	store, err := credentials.Load(path)
	if err != nil {
		return err
	}
	user, err := resolveTokenUser(store, host, options.User)
	if err != nil {
		return err
	}
	credential, ok := store.CredentialForUser(host, user, purpose)
	if !ok {
		return fmt.Errorf("@%s 在 %s 没有 %s 用途令牌（assistant login add %s --user %s 派生；已登记的令牌见 assistant login token list %s）",
			user, host, purpose, host, user, host)
	}
	stdout := command.OutOrStdout()
	fmt.Fprintf(stdout, "站点：%s\n", credential.Host)
	fmt.Fprintf(stdout, "账号：@%s\n", credential.User)
	fmt.Fprintf(stdout, "用途：%s\n", credential.Purpose)
	fmt.Fprintf(stdout, "令牌名：%s\n", orDash(credential.TokenName))
	fmt.Fprintf(stdout, "末 8 位：%s\n", orDash(credentials.LastEight(credential.Token)))
	fmt.Fprintf(stdout, "权限：%s\n", orDash(strings.Join(credential.Scopes, ",")))
	fmt.Fprintf(stdout, "来源：%s\n", orDash(credential.Source))
	fmt.Fprintf(stdout, "创建：%s\n", orDash(credential.CreatedAt))
	fmt.Fprintf(stdout, "令牌：%s\n", credential.Token)
	if options.Check {
		login, isAdmin, err := verifyTokenIdentity(command.Context(), host, credential.Token)
		if err != nil {
			return fmt.Errorf("令牌无效或站点不可达（可尝试 assistant login token refresh %s %s）：%w", host, purpose, err)
		}
		fmt.Fprintf(stdout, "站点校验：有效（@%s", login)
		if isAdmin {
			fmt.Fprint(stdout, "，实例管理员")
		}
		fmt.Fprintln(stdout, "）")
	}
	return nil
}

// tokenRefreshOptions 是 refresh 的输入：密码来源与 add 一致（--password >
// --password-stdin > 终端隐藏输入），账号缺省站点当前登录身份。
type tokenRefreshOptions struct {
	User          string
	Password      string
	PasswordStdin bool
	TOTP          string
}

func newLoginTokenRefreshCommand() *cobra.Command {
	options := &tokenRefreshOptions{}
	command := &cobra.Command{
		Use:   "refresh <host> [purpose]",
		Short: "轮换一条用途令牌：站点上删同名旧令牌重建，新令牌写回凭据库",
		Long: "轮换已登记的用途令牌（Gitea 不回读令牌值，无法原地续期）：用账号密码在\n" +
			"站点上删除同名旧令牌，按记录里的权限集重建，再写回凭据库。令牌名与用途\n" +
			"保持不变；输出不出新令牌明文，需要时用 assistant login token show 查看。\n\n" +
			"密码来源与 add 相同：--password > --password-stdin > 终端隐藏输入。",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			host := strings.TrimRight(strings.TrimSpace(args[0]), "/")
			purpose, err := resolveTokenPurpose(args)
			if err != nil {
				return err
			}
			return runTokenRefresh(command, host, purpose, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.User, "user", "", "令牌所属账号（缺省站点当前登录身份）")
	flags.StringVar(&options.Password, "password", "", "账号密码（仅用于本次轮换，不落盘；建议用 --password-stdin）")
	flags.BoolVar(&options.PasswordStdin, "password-stdin", false, "从标准输入读取密码（非交互环境）")
	flags.StringVar(&options.TOTP, "totp", "", "双因素验证码")
	return command
}

func runTokenRefresh(command *cobra.Command, host, purpose string, options *tokenRefreshOptions) error {
	path, err := credentials.Path()
	if err != nil {
		return err
	}
	store, err := credentials.Load(path)
	if err != nil {
		return err
	}
	user, err := resolveTokenUser(store, host, options.User)
	if err != nil {
		return err
	}
	stored, ok := store.CredentialForUser(host, user, purpose)
	if !ok {
		return fmt.Errorf("@%s 在 %s 没有 %s 用途令牌（assistant login add %s --user %s 派生）",
			user, host, purpose, host, user)
	}
	// 轮换保持令牌名与权限集不变：同名替换是站点侧唯一可行的续期方式。
	name := stored.TokenName
	if name == "" {
		if name, err = credentials.TokenName(host, user, purpose); err != nil {
			return err
		}
	}
	scopes := stored.Scopes
	if len(scopes) == 0 {
		if scopes, err = credentials.DefaultScopes(purpose); err != nil {
			return err
		}
	}
	password, err := readIdentityPassword(command, newPromptSession(command), host, user, &passwordSource{
		Password: options.Password, PasswordStdin: options.PasswordStdin, TOTP: options.TOTP,
	})
	if err != nil {
		return err
	}
	// EnsureUserToken 先删站点上的同名旧令牌再新建：create 在删除之后失败时，
	// 本地凭据库指向的旧令牌已随之失效（被锁在站点外）——replaced>0 时必须把
	// 后果与恢复路径讲清楚，不能只报 create 的错。
	token, replaced, err := setup.EnsureUserToken(
		command.Context(), host, user, password, strings.TrimSpace(options.TOTP), name, scopes)
	if err != nil {
		if replaced > 0 {
			return fmt.Errorf("重建令牌 %s 失败：%w\n站点上 %d 条同名旧令牌已被删除，本地凭据库仍指向失效令牌；"+
				"请重新登录（assistant login add %s --user %s）并在 Gitea Applications 页面检查该令牌",
				name, err, replaced, host, user)
		}
		return fmt.Errorf("重建令牌 %s 失败: %w", name, err)
	}
	if login, _, verifyErr := verifyTokenIdentity(command.Context(), host, token); verifyErr != nil {
		if replaced > 0 {
			return fmt.Errorf("已重建令牌 %s，但身份校验失败，未保存：%v\n站点上 %d 条同名旧令牌已被删除，本地凭据库仍指向失效令牌；"+
				"请重新登录（assistant login add %s --user %s）并在 Gitea Applications 页面检查/撤销该令牌",
				name, verifyErr, replaced, host, user)
		}
		return fmt.Errorf("已重建令牌 %s，但身份校验失败，未保存；请在 Gitea Applications 页面检查该令牌：%v", name, verifyErr)
	} else if login != user {
		message := fmt.Sprintf("令牌 %s 属于 @%s，与 @%s 不一致；未保存，请在 Gitea Applications 页面撤销该令牌", name, login, user)
		if replaced > 0 {
			message += fmt.Sprintf("；站点上 %d 条同名旧令牌已被删除，本地凭据库已失效，请重新登录（assistant login add %s --user %s）",
				replaced, host, user)
		}
		return fmt.Errorf("%s", message)
	}
	// CreatedAt 留空：SetCredential 会盖当前时间，记录的是当前令牌的创建时刻。
	store.SetCredential(credentials.Credential{
		Host: host, User: user, Purpose: purpose,
		Token: token, TokenName: name, LastEight: credentials.LastEight(token),
		Scopes: scopes, Source: cmp.Or(stored.Source, credentials.SourceLogin),
	})
	if err := credentials.Save(path, store); err != nil {
		return fmt.Errorf("写入凭据库失败: %w", err)
	}
	stdout := command.OutOrStdout()
	if replaced > 0 {
		fmt.Fprintf(stdout, "已轮换 %d 条同名旧令牌（%s）\n", replaced, name)
	}
	fmt.Fprintf(stdout, "已重建 %s 用途令牌 %s：@%s @ %s（末 8 位 %s）\n",
		purpose, name, user, host, credentials.LastEight(token))
	fmt.Fprintf(stdout, "  凭据：%s\n", path)
	return nil
}

// defaultTokenScopes 已下沉为 credentials.DefaultScopes：与用途常量同文件登记，
// 新增用途漏配会显式报错而不是静默错配。
