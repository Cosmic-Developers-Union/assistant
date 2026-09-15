package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/repoinstall"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// runIdentityLogin 是 username/password 登录入口：先确定身份（账号名 + 是否实例
// 管理员），再派生/复用该身份在 (host, user, purpose=mcp) 上的长期令牌，写进
// credentials.json。身份与用途令牌都只属于这个 (站点, 账号)，不借用 tea、管理员
// 或机器人凭据，也不与它们混存同一个文件。
func runIdentityLogin(
	command *cobra.Command,
	host, configPath string,
	file *instances.File,
	options *loginOptions,
) error {
	for _, name := range []string{"oauth-client-id", "oauth-client-secret", "oauth-scope", "oauth-port"} {
		if command.Flags().Changed(name) {
			return fmt.Errorf("身份登录使用账号密码或专用令牌，不接受 --%s", name)
		}
	}
	credentialPath, err := credentials.PathFor(configPath)
	if err != nil {
		return err
	}
	store, err := credentials.Load(credentialPath)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(options.Token)
	if options.TokenFile != "" && token != "" {
		return fmt.Errorf("--token 与 --token-file 只能指定一个")
	}
	if options.Password != "" && options.PasswordStdin {
		return fmt.Errorf("--password 与 --password-stdin 只能指定一个")
	}
	importing := token != "" || options.TokenFile != ""
	if importing && (options.Password != "" || options.PasswordStdin || options.TOTP != "" || options.TokenName != "") {
		return fmt.Errorf("录入令牌不能同时使用密码认证参数（--password / --password-stdin / --totp / --token-name）；--user 可作为身份断言")
	}
	if options.TokenFile != "" {
		data, err := os.ReadFile(options.TokenFile)
		if err != nil {
			return fmt.Errorf("读取 --token-file: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return fmt.Errorf("--token-file 文件为空")
		}
	}

	// declaredUser 非空表示本次登录声明了账号身份：校验结果必须一致，否则不落盘。
	declaredUser := strings.TrimSpace(options.User)
	source := "--token"
	switch {
	case options.TokenFile != "":
		source = "token-file"
	case !importing:
		source = "password"
	}

	var (
		// tokenName 是本次生效的令牌名（复用旧凭据时沿用旧名，便于审计）
		tokenName string
		// created 表示本次真的在站点上建了令牌（用于失败提示：令牌已存在但没保存）
		created bool
	)
	mcpToken := token
	if !importing {
		if declaredUser == "" {
			// 只复用同站点 tea 登录的用户名，不取 tea token。
			declaredUser = strings.TrimSpace(repoinstall.InspectTeaLogin(host, os.Getenv).User)
		}
		if declaredUser == "" {
			return fmt.Errorf("请用 --user 指定登录账号，或用 --token/--token-file 录入专用令牌")
		}
		// 已有该身份的 mcp 令牌且仍然有效：直接复用，不重新建令牌（也不需要密码）
		if stored, ok := store.CredentialForUser(host, declaredUser, credentials.PurposeMCP); ok && !options.Rotate {
			if user, _, verifyErr := verifyTokenIdentity(command.Context(), host, stored.Token); verifyErr == nil && user == declaredUser {
				mcpToken, tokenName, source = stored.Token, stored.TokenName, "reused"
			}
		}
		if mcpToken == "" {
			password, err := readIdentityPassword(command, host, declaredUser, options)
			if err != nil {
				return err
			}
			name := strings.TrimSpace(options.TokenName)
			if name == "" {
				name, err = credentials.TokenName(host, declaredUser, credentials.PurposeMCP)
				if err != nil {
					return err
				}
			}
			replaced := 0
			mcpToken, replaced, err = setup.EnsureMCPToken(
				command.Context(), host, declaredUser, password, strings.TrimSpace(options.TOTP), name)
			if err != nil {
				if replaced > 0 {
					return fmt.Errorf("已轮换 %d 个同名旧令牌，但新建失败：%w", replaced, err)
				}
				return err
			}
			tokenName, created = name, true
		}
	}

	// 无论创建还是录入，都用令牌向站点确认身份——声明了账号就是身份断言。
	login, isAdmin, err := verifyTokenIdentity(command.Context(), host, mcpToken)
	if err != nil {
		if created {
			return fmt.Errorf("已创建令牌 %s，但身份校验失败，未保存；请在 Gitea Applications 页面检查该令牌", tokenName)
		}
		return fmt.Errorf("MCP 令牌无法通过站点 %s 的身份校验", host)
	}
	if declaredUser != "" && login != declaredUser {
		if created {
			return fmt.Errorf("账号 @%s 的令牌创建成功（名称 %s），但校验得到身份 @%s；未保存，请在 Gitea Applications 页面撤销该令牌",
				declaredUser, tokenName, login)
		}
		return fmt.Errorf("MCP 令牌属于 @%s，与声明的 --user @%s 不一致；未保存（换账号请分别登录）", login, declaredUser)
	}

	store.SetIdentity(credentials.Identity{Host: host, User: login, IsAdmin: isAdmin})
	store.SetCredential(credentials.Credential{
		Host: host, User: login, Purpose: credentials.PurposeMCP,
		Token: mcpToken, TokenName: tokenName, LastEight: credentials.LastEight(mcpToken),
		Scopes: credentials.MCPScopes(), Source: source,
	})
	if err := credentials.Save(credentialPath, store); err != nil {
		if created {
			return fmt.Errorf("已创建令牌 %s，但写入凭据库失败；请在 Gitea Applications 页面检查该令牌: %w", tokenName, err)
		}
		return fmt.Errorf("写入凭据库失败: %w", err)
	}
	// 旧版把 mcp 令牌写在 config.json 的实例条目里：凭据库接管后清掉，避免两处副本
	if clearLegacyMCPCredential(file, host) {
		if err := saveLoginFile(file, configPath); err != nil {
			return err
		}
	}

	stdout := command.OutOrStdout()
	fmt.Fprintf(stdout, "已登录 %s @%s（mcp 令牌 %s，凭据 %s）\n", host, login, tokenName, credentialPath)
	if isAdmin {
		fmt.Fprintf(stdout, "身份已记录：@%s 是实例管理员，setup/init/actions 等管理操作可用\n", login)
	} else {
		fmt.Fprintf(stdout, "身份已记录：@%s 不是实例管理员，仅 MCP 工具面可用；"+
			"setup/init/actions 与机器人凭据需要管理员账号\n", login)
	}
	return nil
}

// verifyTokenIdentity 用令牌向站点确认账号名与管理员身份。
func verifyTokenIdentity(ctx context.Context, host, token string) (string, bool, error) {
	client, err := status.NewClient(host, token)
	if err != nil {
		return "", false, err
	}
	return client.AuthenticatedIdentity(ctx)
}

// readIdentityPassword 取本次认证用的密码：--password 显式值 > 标准输入 > 终端
// 隐藏输入。非交互环境不给交互式等待的机会（stdio MCP/CI 会挂住）。
func readIdentityPassword(
	command *cobra.Command,
	host, user string,
	options *loginOptions,
) (string, error) {
	if options.Password != "" {
		return options.Password, nil
	}
	if options.PasswordStdin {
		data, err := io.ReadAll(io.LimitReader(command.InOrStdin(), 64*1024+1))
		if err != nil || len(data) > 64*1024 {
			return "", fmt.Errorf("无法读取密码或密码输入过长")
		}
		return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("非交互环境请用 --password-stdin 传入密码，或 --token/--token-file 录入专用令牌")
	}
	fmt.Fprintf(command.ErrOrStderr(), "请输入 %s 上 @%s 的密码：", host, user)
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(command.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("读取密码失败: %w", err)
	}
	return string(data), nil
}

// clearLegacyMCPCredential 清掉 config.json 里的旧版 mcp_token/mcp_user（凭据库
// 已接管），返回是否有改动。
func clearLegacyMCPCredential(file *instances.File, host string) bool {
	if file == nil {
		return false
	}
	changed := false
	for index := range file.Instances {
		if !sameHost(file.Instances[index].Host, host) {
			continue
		}
		if file.Instances[index].MCPToken == "" && file.Instances[index].MCPUser == "" {
			continue
		}
		file.Instances[index].MCPToken = ""
		file.Instances[index].MCPUser = ""
		changed = true
	}
	return changed
}
