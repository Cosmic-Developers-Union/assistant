package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"assistant/internal/instances"
	"assistant/internal/repoinstall"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func runMCPLogin(command *cobra.Command, host, path string, file *instances.File, options *loginOptions) error {
	for _, name := range []string{"oauth-client-id", "oauth-client-secret", "oauth-scope", "oauth-port"} {
		if command.Flags().Changed(name) {
			return fmt.Errorf("--mcp 使用长期个人令牌，不接受 --%s", name)
		}
	}
	token := strings.TrimSpace(options.Token)
	if options.TokenFile != "" && token != "" {
		return fmt.Errorf("--token 与 --token-file 只能指定一个")
	}
	importing := token != "" || options.TokenFile != ""
	if importing && (options.PasswordStdin || options.TOTP != "" || options.TokenName != "") {
		return fmt.Errorf("录入令牌不能同时使用密码认证参数（--password-stdin / --totp / --token-name）；--user 可作为身份断言")
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
	// expectUser 非空表示本次登录声明了账号身份：身份校验必须一致，否则不落盘。
	expectUser := strings.TrimSpace(options.User)
	createdName := ""
	if !importing {
		if expectUser == "" {
			// 只复用同站点 tea 登录的用户名，不取 tea token。
			expectUser = strings.TrimSpace(repoinstall.InspectTeaLogin(host, os.Getenv).User)
		}
		if expectUser == "" {
			return fmt.Errorf("请用 --user 指定个人账号用户名，或用 --token-file 录入专用令牌")
		}
		var password string
		if options.PasswordStdin {
			data, err := io.ReadAll(io.LimitReader(command.InOrStdin(), 64*1024+1))
			if err != nil || len(data) > 64*1024 {
				return fmt.Errorf("无法读取密码或密码输入过长")
			}
			password = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		} else {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("非交互环境请用 --password-stdin 传入密码，或 --token-file 录入令牌")
			}
			fmt.Fprintf(command.ErrOrStderr(), "请输入 %s 上 @%s 的密码：", host, expectUser)
			data, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(command.ErrOrStderr())
			if err != nil {
				return fmt.Errorf("读取密码失败: %w", err)
			}
			password = string(data)
		}
		name := strings.TrimSpace(options.TokenName)
		if name == "" {
			raw := make([]byte, 8)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			name = "assistant-mcp-" + hex.EncodeToString(raw)
		}
		var err error
		token, err = setup.CreateMCPToken(command.Context(), host, expectUser, password, strings.TrimSpace(options.TOTP), name)
		if err != nil {
			return err
		}
		createdName = name
	}
	client, err := status.NewClient(host, token)
	if err != nil {
		return err
	}
	login, err := client.AuthenticatedUser(command.Context())
	if err != nil {
		if createdName != "" {
			return fmt.Errorf("已创建令牌 %s，但身份校验失败，未保存；请在 Gitea Applications 页面检查该令牌", createdName)
		}
		return fmt.Errorf("MCP 令牌无法通过站点 %s 的身份校验", host)
	}
	if expectUser != "" && login != expectUser {
		if createdName != "" {
			return fmt.Errorf("账号 @%s 的令牌创建成功（名称 %s），但校验得到身份 @%s；未保存，请在 Gitea Applications 页面撤销该令牌", expectUser, createdName, login)
		}
		return fmt.Errorf("MCP 令牌属于 @%s，与声明的 --user @%s 不一致；未保存（换账号请分别登录）", login, expectUser)
	}
	upsertLoginInstance(file, host, func(instance *instances.Instance) {
		instance.MCPToken = token
		instance.MCPUser = login
	})
	if err := saveLoginFile(file, path); err != nil {
		if createdName != "" {
			return fmt.Errorf("已创建令牌 %s，但保存失败；请在 Gitea Applications 页面检查该令牌: %w", createdName, err)
		}
		return err
	}
	fmt.Fprintf(command.OutOrStdout(), "MCP 已登录 %s @%s（专用长期令牌保存至 %s）\n", host, login, path)
	return nil
}
