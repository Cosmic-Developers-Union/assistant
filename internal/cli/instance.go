package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"uuid"

	sdk "gitea.dev/sdk"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/qq"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/telegram"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/weixin"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func parseRepo(raw string) (string, string, error) { return credentials.ParseRepoName(raw) }

type instanceRow struct{ Name, Type, Address, Account string }

func rows(file *credentials.File) []instanceRow {
	var result []instanceRow
	for _, v := range file.Instances.Gitea {
		result = append(result, instanceRow{v.Name, "gitea", v.URL, v.Username})
	}
	for _, v := range file.Instances.QQ {
		result = append(result, instanceRow{v.Name, "qq", "", v.AppID})
	}
	for _, v := range file.Instances.Weixin {
		result = append(result, instanceRow{v.Name, "weixin", v.URL, v.UserID})
	}
	for _, v := range file.Instances.Telegram {
		result = append(result, instanceRow{v.Name, "telegram", "", v.Username})
	}
	slices.SortFunc(result, func(a, b instanceRow) int { return strings.Compare(a.Name, b.Name) })
	return result
}

type instanceLogins struct {
	Gitea    func(context.Context, string, string, string, string, string) (string, error)
	QQ       func(context.Context, string, string) error
	Telegram func(context.Context, string) (string, error)
	Weixin   func(context.Context, string, func(weixin.QRCode), func(int) (string, error)) (weixin.Credentials, error)
}

func defaultInstanceLogins() instanceLogins {
	return instanceLogins{
		Gitea: loginGitea,
		QQ: func(ctx context.Context, id, secret string) error {
			return credentials.RedactError(qq.NewClient(qq.Config{AppID: id, AppSecret: secret}).VerifyCredential(ctx), secret)
		},
		Telegram: func(ctx context.Context, token string) (string, error) {
			me, err := telegram.NewClient(telegram.Config{BotToken: token}).GetMe(ctx)
			return me.Username, credentials.RedactError(err, token)
		},
		Weixin: func(ctx context.Context, url string, qr func(weixin.QRCode), verify func(int) (string, error)) (weixin.Credentials, error) {
			return weixin.Login(ctx, url, qr, verify, nil)
		},
	}
}
func newInstanceCommand() *cobra.Command {
	return newInstanceCommandWithLogins(defaultInstanceLogins())
}
func newInstanceCommandWithLogins(logins instanceLogins) *cobra.Command {
	root := &cobra.Command{Use: "instance", Short: "管理用户级平台连接（与 run 配置独立）", Args: cobra.NoArgs}
	load := func() (string, *credentials.File, error) {
		path, err := credentials.Path()
		if err != nil {
			return "", nil, err
		}
		file, err := credentials.Load(path)
		return path, file, err
	}
	list := &cobra.Command{Use: "list", Short: "列出已添加实例", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, file, err := load()
		if err != nil {
			return err
		}
		entries := rows(file)
		if len(entries) == 0 {
			cmd.Println("尚未添加实例：assistant instance add <平台>")
		}
		for _, row := range entries {
			cmd.Printf("%s\t%s\t%s\t%s\n", row.Name, row.Type, row.Address, row.Account)
		}
		return nil
	}}
	show := &cobra.Command{Use: "show <实例名>", Short: "查看实例身份（不输出密钥）", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		_, file, err := load()
		if err != nil {
			return err
		}
		for _, row := range rows(file) {
			if row.Name == args[0] {
				cmd.Printf("名称: %s\n平台: %s\n地址: %s\n账号: %s\n", row.Name, row.Type, row.Address, row.Account)
				return nil
			}
		}
		return fmt.Errorf("实例 %s 不存在", args[0])
	}}
	remove := &cobra.Command{Use: "remove <实例名>", Short: "删除本地条目，保留平台账号与令牌", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, file, err := load()
		if err != nil {
			return err
		}
		before := len(rows(file))
		name := args[0]
		file.Instances.Gitea = slices.DeleteFunc(file.Instances.Gitea, func(v credentials.Gitea) bool { return v.Name == name })
		file.Instances.QQ = slices.DeleteFunc(file.Instances.QQ, func(v credentials.QQ) bool { return v.Name == name })
		file.Instances.Weixin = slices.DeleteFunc(file.Instances.Weixin, func(v credentials.Weixin) bool { return v.Name == name })
		file.Instances.Telegram = slices.DeleteFunc(file.Instances.Telegram, func(v credentials.Telegram) bool { return v.Name == name })
		if before == len(rows(file)) {
			return fmt.Errorf("实例 %s 不存在", name)
		}
		if err := credentials.Save(path, file); err != nil {
			return err
		}
		cmd.Printf("已移除本地实例 %s\n", name)
		return nil
	}}
	add := &cobra.Command{Use: "add", Short: "添加平台连接，缺失参数时交互询问", Args: cobra.NoArgs}
	for _, kind := range []string{"gitea", "qq", "weixin", "telegram"} {
		var name, address, user, passwordFile, tokenFile, appID, secretFile, otp string
		cmd := &cobra.Command{Use: kind, Short: "添加 " + kind + " 实例", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			path, file, err := load()
			if err != nil {
				return err
			}
			prompts := newPrompts(cmd)
			if name, err = prompts.value(name, "实例名"); err != nil {
				return err
			}
			for _, row := range rows(file) {
				if row.Name == name {
					return fmt.Errorf("实例 %s 已存在，先 remove 后重新添加", name)
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			switch kind {
			case "gitea":
				if address, err = prompts.value(address, "Gitea 地址"); err != nil {
					return err
				}
				if err := credentials.ValidateHost(address); err != nil {
					return err
				}
				if user, err = prompts.value(user, "账号"); err != nil {
					return err
				}
				password, err := prompts.secret(passwordFile, "密码")
				if err != nil {
					return err
				}
				token, err := logins.Gitea(ctx, address, user, password, otp, name)
				if err != nil {
					return err
				}
				file.Instances.Gitea = append(file.Instances.Gitea, credentials.Gitea{Name: name, URL: credentials.NormalizeHost(address), Username: user, Token: token})
			case "qq":
				if appID, err = prompts.value(appID, "AppID"); err != nil {
					return err
				}
				secret, err := prompts.secret(secretFile, "AppSecret")
				if err != nil {
					return err
				}
				if err := logins.QQ(ctx, appID, secret); err != nil {
					return err
				}
				file.Instances.QQ = append(file.Instances.QQ, credentials.QQ{Name: name, AppID: appID, AppSecret: secret})
			case "telegram":
				token, err := prompts.secret(tokenFile, "Bot token")
				if err != nil {
					return err
				}
				username, err := logins.Telegram(ctx, token)
				if err != nil {
					return err
				}
				file.Instances.Telegram = append(file.Instances.Telegram, credentials.Telegram{Name: name, Username: username, Token: token})
			case "weixin":
				result, err := logins.Weixin(ctx, address, func(code weixin.QRCode) {
					if err := weixin.RenderQR(cmd.ErrOrStderr(), code.Content); err != nil {
						fmt.Fprintln(cmd.ErrOrStderr(), "扫码登录:", code.Content)
					}
				}, func(int) (string, error) { return prompts.value("", "验证码") })
				if err != nil {
					return err
				}
				file.Instances.Weixin = append(file.Instances.Weixin, credentials.Weixin{Name: name, URL: result.BaseURL, UserID: result.UserID, BotID: result.BotID, Token: result.BotToken})
			}
			if err := credentials.Save(path, file); err != nil {
				return fmt.Errorf("平台登录成功，保存本地实例失败: %w", err)
			}
			cmd.Printf("已添加 %s 实例 %s\n", kind, name)
			return nil
		}}
		flags := cmd.Flags()
		flags.StringVar(&name, "name", "", "实例名")
		switch kind {
		case "gitea":
			flags.StringVar(&address, "url", "", "站点地址")
			flags.StringVar(&user, "username", "", "账号")
			flags.StringVar(&passwordFile, "password-file", "", "密码文件（不会落入 shell 历史）")
			flags.StringVar(&otp, "otp", "", "两步验证的当前验证码")
		case "weixin":
			flags.StringVar(&address, "url", "", "扫码平台地址（缺省官方地址）")
		case "qq":
			flags.StringVar(&appID, "app-id", "", "开放平台 AppID")
			flags.StringVar(&secretFile, "app-secret-file", "", "AppSecret 文件")
		case "telegram":
			flags.StringVar(&tokenFile, "token-file", "", "BotFather token 文件")
		}
		add.AddCommand(cmd)
	}
	root.AddCommand(list, show, remove, add)
	return root
}

type prompts struct {
	command *cobra.Command
	reader  *bufio.Reader
	input   *os.File
}

func newPrompts(cmd *cobra.Command) *prompts {
	input, _ := cmd.InOrStdin().(*os.File)
	return &prompts{command: cmd, reader: bufio.NewReader(cmd.InOrStdin()), input: input}
}
func (p *prompts) value(value, label string) (string, error) {
	if value != "" {
		return value, nil
	}
	if p.input == nil || !term.IsTerminal(int(p.input.Fd())) {
		return "", fmt.Errorf("缺少 %s，请使用对应选项（非交互环境不询问）", label)
	}
	fmt.Fprintf(p.command.ErrOrStderr(), "%s: ", label)
	result, err := p.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("读取 %s: %w", label, err)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return "", fmt.Errorf("%s 不能为空", label)
	}
	return result, nil
}
func (p *prompts) secret(path, label string) (string, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取 %s 文件: %w", label, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("%s 文件为空", label)
		}
		return value, nil
	}
	if p.input == nil || !term.IsTerminal(int(p.input.Fd())) {
		return "", fmt.Errorf("缺少 %s，请使用密钥文件选项", label)
	}
	fmt.Fprintf(p.command.ErrOrStderr(), "%s: ", label)
	data, err := term.ReadPassword(int(p.input.Fd()))
	fmt.Fprintln(p.command.ErrOrStderr())
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("%s 不能为空", label)
	}
	return string(data), nil
}

func loginGitea(ctx context.Context, host, user, password, otp, name string) (string, error) {
	client, err := sdk.NewClient(host, sdk.SetBasicAuth(user, password), sdk.SetOTP(otp), sdk.SetGiteaVersion(""), sdk.SetHTTPClient(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	if err != nil {
		return "", err
	}
	me, _, err := client.Users.GetMyUserInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("Gitea 登录失败: %w", err)
	}
	if me.UserName != user {
		return "", fmt.Errorf("Gitea 返回的账号身份不匹配")
	}
	token, _, err := client.Users.CreateAccessToken(ctx, sdk.CreateAccessTokenOption{Name: "assistant-instance-" + name + "-" + uuid.New().String(), Scopes: []sdk.AccessTokenScope{"read:repository", "write:repository", "read:issue", "write:issue", "read:user"}})
	if err != nil {
		return "", fmt.Errorf("创建个人工具令牌: %w", err)
	}
	return token.Token, nil
}
