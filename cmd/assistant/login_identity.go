package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// runIdentityLogin 是唯一的登录实现：host + username + password → 用站点确认身份
// （账号名 + 是否实例管理员）→ 按身份派生用途令牌 → 写入凭据库。
//
// 派生规则：mcp 必有（编辑器/CLI 工具面）；admin 仅当账号是实例管理员（管理操作）。
// 没有 tea / OAuth / 令牌录入等平行路径，也没有第二处凭据落点。
func runIdentityLogin(command *cobra.Command, prompts *promptSession, host, configPath string, options *loginOptions) error {
	writePath, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	if file == nil {
		file = &instances.File{}
	}
	credentialPath, err := credentials.Path()
	if err != nil {
		return err
	}
	store, err := credentials.Load(credentialPath)
	if err != nil {
		return err
	}

	user := strings.TrimSpace(options.User)
	if user == "" {
		if user, err = prompts.line("Gitea 账号："); err != nil {
			return fmt.Errorf("缺少账号：%w", err)
		}
	}
	if user == "" {
		return fmt.Errorf("缺少账号（--user 或交互式输入）")
	}

	// 密码按需读取：已存令牌仍然有效时，重复登录连密码都不用输。
	var (
		password     string
		havePassword bool
	)
	passwordFor := func() (string, error) {
		if havePassword {
			return password, nil
		}
		value, err := readIdentityPassword(command, prompts, host, user, options)
		if err != nil {
			return "", err
		}
		password, havePassword = value, true
		return value, nil
	}

	// 1) mcp 令牌：优先复用已存且有效的令牌
	storedMCP, hasStoredMCP := store.CredentialForUser(host, user, credentials.PurposeMCP)
	mcpToken, mcpName := "", ""
	login, isAdmin := "", false
	if hasStoredMCP && !options.Rotate {
		if name, admin, verifyErr := verifyTokenIdentity(command.Context(), host, storedMCP.Token); verifyErr == nil && name == user {
			mcpToken, mcpName, login, isAdmin = storedMCP.Token, storedMCP.TokenName, name, admin
		}
	}
	createdNames := []string{}
	if mcpToken == "" {
		secret, err := passwordFor()
		if err != nil {
			return err
		}
		name := strings.TrimSpace(options.TokenName)
		if name == "" {
			if name, err = credentials.TokenName(host, user, credentials.PurposeMCP); err != nil {
				return err
			}
		}
		token, replaced, err := setup.EnsureUserToken(
			command.Context(), host, user, secret, strings.TrimSpace(options.TOTP), name, credentials.MCPScopes())
		if err != nil {
			return err
		}
		if replaced > 0 {
			fmt.Fprintf(command.ErrOrStderr(), "已轮换 %d 条同名旧令牌（%s）\n", replaced, name)
		}
		mcpToken, mcpName = token, name
		createdNames = append(createdNames, name)
		if login, isAdmin, err = verifyTokenIdentity(command.Context(), host, mcpToken); err != nil {
			return fmt.Errorf("已创建令牌 %s，但身份校验失败，未保存；请在 Gitea Applications 页面检查该令牌", name)
		}
		if login != user {
			return fmt.Errorf("账号 @%s 的令牌创建成功（名称 %s），但校验得到身份 @%s；未保存，请在 Applications 页面撤销该令牌",
				user, name, login)
		}
	}

	// 2) admin 令牌：只有实例管理员需要（setup/init/actions 用它）
	adminCredential, err := ensureAdminCredential(command, host, user, isAdmin, store, passwordFor, options, &createdNames)
	if err != nil {
		return err
	}

	// 3) 落盘：凭据库（唯一凭据落点）+ config.json 的平台条目
	store.SetIdentity(credentials.Identity{Host: host, User: login, IsAdmin: isAdmin})
	store.SetCredential(credentials.Credential{
		Host: host, User: login, Purpose: credentials.PurposeMCP,
		Token: mcpToken, TokenName: mcpName, LastEight: credentials.LastEight(mcpToken),
		Scopes: credentials.MCPScopes(), Source: credentials.SourceLogin,
	})
	if adminCredential != nil {
		store.SetCredential(*adminCredential)
	}
	if err := credentials.Save(credentialPath, store); err != nil {
		if len(createdNames) > 0 {
			return fmt.Errorf("已创建令牌 %s，但写入凭据库失败；请在 Gitea Applications 页面检查该令牌: %w",
				strings.Join(createdNames, "、"), err)
		}
		return fmt.Errorf("写入凭据库失败: %w", err)
	}
	upsertLoginInstance(file, host)
	if err := saveLoginFile(file, writePath); err != nil {
		return err
	}

	stdout := command.OutOrStdout()
	fmt.Fprintf(stdout, "已登录 %s @%s\n", host, login)
	fmt.Fprintf(stdout, "  mcp 令牌：%s（编辑器/CLI 的 MCP 工具面）\n", mcpName)
	if adminCredential != nil {
		fmt.Fprintf(stdout, "  admin 令牌：%s（setup/init/actions 管理操作）\n", adminCredential.TokenName)
	} else {
		fmt.Fprintf(stdout, "  账号 @%s 不是实例管理员：只有 mcp 能力；管理操作需要管理员账号登录\n", login)
	}
	fmt.Fprintf(stdout, "  凭据：%s\n", credentialPath)
	return nil
}

// ensureAdminCredential 保证管理员账号有 purpose=admin 令牌：已存且有效则复用，
// 否则用密码新建。非管理员返回 nil（不派生该用途）。
func ensureAdminCredential(
	command *cobra.Command,
	host, user string,
	isAdmin bool,
	store *credentials.File,
	passwordFor func() (string, error),
	options *loginOptions,
	createdNames *[]string,
) (*credentials.Credential, error) {
	if !isAdmin {
		return nil, nil
	}
	if stored, ok := store.CredentialForUser(host, user, credentials.PurposeAdmin); ok && !options.Rotate {
		if name, _, verifyErr := verifyTokenIdentity(command.Context(), host, stored.Token); verifyErr == nil && name == user {
			return &stored, nil
		}
	}
	secret, err := passwordFor()
	if err != nil {
		return nil, err
	}
	name, err := credentials.TokenName(host, user, credentials.PurposeAdmin)
	if err != nil {
		return nil, err
	}
	token, replaced, err := setup.EnsureUserToken(
		command.Context(), host, user, secret, strings.TrimSpace(options.TOTP), name, credentials.AdminScopes())
	if err != nil {
		return nil, err
	}
	if replaced > 0 {
		fmt.Fprintf(command.ErrOrStderr(), "已轮换 %d 条同名旧令牌（%s）\n", replaced, name)
	}
	*createdNames = append(*createdNames, name)
	if login, _, verifyErr := verifyTokenIdentity(command.Context(), host, token); verifyErr != nil {
		return nil, fmt.Errorf("已创建令牌 %s，但身份校验失败，未保存；请在 Gitea Applications 页面检查该令牌", name)
	} else if login != user {
		return nil, fmt.Errorf("令牌 %s 属于 @%s，与 @%s 不一致；未保存", name, login, user)
	}
	return &credentials.Credential{
		Host: host, User: user, Purpose: credentials.PurposeAdmin,
		Token: token, TokenName: name, LastEight: credentials.LastEight(token),
		Scopes: credentials.AdminScopes(), Source: credentials.SourceLogin,
	}, nil
}

// verifyTokenIdentity 用令牌向站点确认账号名与管理员身份。
func verifyTokenIdentity(ctx context.Context, host, token string) (string, bool, error) {
	client, err := status.NewClient(host, token)
	if err != nil {
		return "", false, err
	}
	return client.AuthenticatedIdentity(ctx)
}

// readIdentityPassword 取本次派生令牌用的密码：--password > --password-stdin >
// 终端隐藏输入（测试等注入输入源时按行读取）。非交互环境绝不等待。
func readIdentityPassword(
	command *cobra.Command,
	prompts *promptSession,
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
		value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		if value == "" {
			return "", fmt.Errorf("读取到的密码为空")
		}
		return value, nil
	}
	if !prompts.interactive() {
		return "", fmt.Errorf("非交互环境请用 --password-stdin 提供密码")
	}
	if command.InOrStdin() == os.Stdin && isTerminal(os.Stdin) {
		fmt.Fprintf(command.ErrOrStderr(), "请输入 %s 上 @%s 的密码：", host, user)
		data, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(command.ErrOrStderr())
		if err != nil {
			return "", fmt.Errorf("读取密码失败: %w", err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("密码为空")
		}
		return string(data), nil
	}
	// 注入的输入源（测试/管道）：按行读取
	value, err := prompts.secret(fmt.Sprintf("请输入 %s 上 @%s 的密码：", host, user))
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("密码为空")
	}
	return value, nil
}

// isTerminal 判断 fd 是否终端（交互式询问/隐藏输入的前提）。
func isTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
}
