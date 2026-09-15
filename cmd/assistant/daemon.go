package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"assistant/internal/claudecfg"
	"assistant/internal/daemon"
	"assistant/internal/instances"
	"assistant/internal/provider"
	"assistant/internal/weixin"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newDaemonMCPCommand 是自举的 daemon 状态 MCP：自己发现运行中的 assistant run
// （端点文件 / ASSISTANT_DAEMON_ENDPOINT），工具全部只读。
func newDaemonMCPCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "自举的 daemon 状态 MCP（自动发现运行中的 assistant run，只读）",
		Long: "以 stdio 提供 daemon 状态查询工具：\n" +
			"  daemon_status / list_sessions / list_queue / recent_results\n" +
			"自动从 " + endpointPathHint() + " 发现运行中的 assistant run（可用\n" +
			"ASSISTANT_DAEMON_ENDPOINT 覆盖路径、ASSISTANT_DAEMON_ADDR 覆盖地址），\n" +
			"无需在 MCP 配置里写地址或令牌；daemon 未运行时工具返回可读错误。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return daemon.RunMCP(command.Context(), os.Stdin, os.Stdout, os.Getenv, version)
		},
	}
}

func endpointPathHint() string {
	path, err := instances.DaemonEndpointPath()
	if err != nil {
		return "daemon.json"
	}
	return path
}

type weixinLoginOptions struct {
	BaseURL string
}

// newWeixinCommand 管理微信对话桥：扫码登录（换 Bot token 写入 config.json）。
func newWeixinCommand(configFlag *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "weixin",
		Short: "微信对话桥（openclaw ilink 协议）：扫码登录与状态",
		Args:  cobra.NoArgs,
	}
	loginOptions := &weixinLoginOptions{}
	loginCommand := &cobra.Command{
		Use:   "login",
		Short: "扫码登录微信 Bot，把凭据写入 config.json 的 weixin 节",
		Long: "按 openclaw-weixin（ilink）协议拉起扫码登录：终端展示二维码内容，\n" +
			"手机扫码确认后把 Bot token 写入 config.json；之后 assistant run 即可\n" +
			"通过微信与 daemon 对话（--weixin 或 weixin.enabled=true）。\n" +
			"需要验证码时会提示输入手机上显示的验证码。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runWeixinLogin(command, *configFlag, loginOptions)
		},
	}
	loginCommand.Flags().StringVar(&loginOptions.BaseURL, "base-url", "",
		"ilink API 根地址（缺省 "+instances.DefaultWeixinBaseURL+"）")
	statusCommand := &cobra.Command{
		Use:   "status",
		Short: "显示微信桥配置状态（不显示令牌）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			if file == nil || file.Weixin == nil {
				fmt.Fprintln(stdout, "未配置微信桥（先 assistant weixin login）")
				return nil
			}
			config := file.Weixin
			login := config.LoginUserID
			if login == "" {
				login = "-"
			}
			fmt.Fprintf(stdout, "enabled=%v base_url=%s login=%s bot_id=%s token=%s admins=%d\n",
				config.Enabled, config.BaseURL, login, orDash(config.BotID), tokenState(config.BotToken), len(config.AdminUsers))
			return nil
		},
	}
	command.AddCommand(loginCommand, statusCommand)
	return command
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func tokenState(token string) string {
	if strings.TrimSpace(token) == "" {
		return "none"
	}
	return "set"
}

func runWeixinLogin(command *cobra.Command, configPath string, options *weixinLoginOptions) error {
	stdout := command.OutOrStdout()
	baseURL := strings.TrimSpace(options.BaseURL)
	if baseURL == "" {
		baseURL = instances.DefaultWeixinBaseURL
	}
	writePath, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	if file == nil {
		file = &instances.File{}
	}
	onQR := func(code weixin.QRCode) {
		fmt.Fprintln(stdout, "请用手机微信扫描下方二维码完成登录（扫码后手机上会提示确认）：")
		if err := weixin.RenderQR(stdout, code.Content); err != nil {
			fmt.Fprintf(stdout, "（终端二维码渲染失败：%v，请使用下方链接）\n", err)
		}
		fmt.Fprintf(stdout, "若二维码无法显示或无法扫描，可访问：%s\n", code.Content)
	}
	promptVerify := func(attempt int) (string, error) {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", fmt.Errorf("需要验证码但当前环境不可交互（请在终端运行）")
		}
		fmt.Fprintf(os.Stderr, "请输入手机显示的验证码（第 %d 次）：", attempt)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	credentials, err := weixin.Login(command.Context(), baseURL, onQR, promptVerify, nil)
	if err != nil {
		return err
	}
	if file.Weixin == nil {
		file.Weixin = &instances.Weixin{}
	}
	weixinConfig := file.Weixin
	weixinConfig.Enabled = true
	weixinConfig.BaseURL = credentials.BaseURL
	weixinConfig.BotToken = credentials.BotToken
	weixinConfig.BotID = credentials.BotID
	weixinConfig.LoginUserID = credentials.UserID
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	if err := instances.Save(writePath, file); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "登录成功：bot_id=%s login=%s，凭据已写入 %s（0600）\n",
		orDash(credentials.BotID), orDash(credentials.UserID), writePath)
	fmt.Fprintln(stdout, "启动对话：assistant run（或 --weixin 强制开启）")
	return nil
}

// startDaemonServices 启动 daemon 模式的服务面：只读状态 API（默认
// 127.0.0.1:8770）与可选的微信对话桥。API 先就绪——对话会话的 daemon MCP
// 依赖端点文件自举发现。
func startDaemonServices(
	command *cobra.Command,
	configPath string,
	options *dispatcherOptions,
	store *daemon.Store,
) error {
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), "[daemon %s] %s\n",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fmt.Sprintf(format, arguments...))
	}
	listen := strings.TrimSpace(options.APIListen)
	if listen != "" && !strings.EqualFold(listen, "none") && !strings.EqualFold(listen, "off") {
		if _, err := daemon.Serve(command.Context(), listen, store, version, logf); err != nil {
			return err
		}
	}

	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil || file.Weixin == nil {
		if options.Weixin {
			return fmt.Errorf("--weixin 需要 config.json 的 weixin 配置：先 assistant weixin login")
		}
		// 静默跳过是最难排查的一种：明确说清桥没起以及为什么
		logf("微信桥未启用：config.json 里没有 weixin 配置（需要时先 `assistant weixin login` 扫码）")
		return nil
	}
	weixinConfig := file.Weixin
	if !options.Weixin && !weixinConfig.Enabled {
		logf("微信桥未启用：weixin.enabled=false（`assistant weixin login` 会置为 true；--weixin 可强制开启）")
		return nil
	}
	if strings.TrimSpace(weixinConfig.BotToken) == "" {
		return fmt.Errorf("weixin.bot_token 未配置：先 assistant weixin login")
	}
	// 对话会话与评审会话共用同一 provider 体系：weixin.provider > 全局默认；
	// 全局 optimizations 打底，provider 覆盖其上
	providerName := file.WeixinProviderName()
	providerOverrides, err := file.EffectiveOverrides(providerName)
	if err != nil {
		return fmt.Errorf("微信桥 provider %s: %w", providerName, err)
	}
	if env, settings, mcp := providerOverrides.Counts(); env+settings+mcp > 0 {
		name := providerName
		if name == "" {
			name = "内置缺省"
		} else if provider.HasPreset(name) {
			name += " 内置预设"
		}
		globalEnv, globalSettings, globalMCP := file.Optimizations.Overrides().Counts()
		global := ""
		if globalEnv+globalSettings+globalMCP > 0 {
			global = fmt.Sprintf("（含全局优化 env %d 项，settings %d 项，mcp %d 个）",
				globalEnv, globalSettings, globalMCP)
		}
		logf("微信桥使用 provider %s（env %d 项，settings %d 项，mcp %d 个）%s", name, env, settings, mcp, global)
	}
	timeout := time.Duration(weixinConfig.SessionTimeoutMS) * time.Millisecond
	claudeBin := firstNonEmpty(weixinConfig.ClaudeBin, options.ClaudeBin, "claude")
	// 对话会话走 claude 的最小模式（--bare）：上下文本来就全由显式参数给出，
	// 最小模式顺带甩掉 hooks、插件同步、CLAUDE.md 自动发现与记忆。仅在 claude
	// 真的支持该参数时打开，不支持就退回普通模式，不让整条桥起不来。
	bare := claudecfg.SupportsBare(claudeBin)
	chat, err := daemon.NewChat(daemon.ChatConfig{
		ClaudeBin:    claudeBin,
		Bare:         bare,
		Debug:        options.Debug,
		Model:        firstNonEmpty(weixinConfig.Model, options.Model),
		Provider:     providerOverrides,
		ProviderName: providerName,
		Timeout:      timeout,
		Log:          logf,
	})
	if err != nil {
		return err
	}
	// 对话会话的前提同样一次说清：claude 与最小模式是否生效、凭据从哪来
	bareLabel := "关（claude 不支持 --bare 或未探测到）"
	if chat.Bare() {
		bareLabel = "开（不读 hooks/插件/CLAUDE.md，只用显式 settings 与自举 MCP）"
	}
	logf("微信桥对话会话：claude=%s bare=%s 配置根=%s", claudeBin, bareLabel, chat.SessionDir())
	logf("微信桥会话目录：%s（每个微信会话一个稳定工作目录 <chat-xxxxxxxx>，含 session.json；"+
		"续聊：cd 该目录后 claude --continue）", chat.StateDir())
	warnf := func(format string, arguments ...any) {
		fmt.Fprintf(command.ErrOrStderr(), "警告："+format+"\n", arguments...)
	}
	if source := claudecfg.CredentialSource(providerOverrides); source == "" {
		warnf("微信桥 provider 没有可用的 AI 凭据：对话会认证失败；%s", claudecfg.MissingCredentialHint)
	} else {
		// 最小请求实测：密钥/端点不配套（如国内 MiniMax 账号配了国际端点）会
		// 让每条微信消息静默重试几分钟，这里启动就说清楚
		check := provider.CheckCredential(command.Context(), providerOverrides)
		if check.OK() {
			logf("微信桥 AI 凭据：%s；%s", source, check.Describe())
		} else {
			warnf("微信桥凭据自检失败：%s", check.Describe())
			warnf("%s", provider.CredentialHint)
		}
	}
	bridgeConfig := daemon.BridgeConfig{
		Weixin: weixin.Config{
			BaseURL:        weixinConfig.BaseURL,
			BotToken:       weixinConfig.BotToken,
			BotAgent:       weixinConfig.BotAgent,
			ChannelVersion: firstNonEmpty(weixinConfig.ChannelVersion, version),
			RouteTag:       weixinConfig.RouteTag,
		},
		AdminUsers:  weixinConfig.AdminUsers,
		LoginUserID: weixinConfig.LoginUserID,
		Chat:        chat,
		Log:         logf,
		Debug:       options.Debug,
	}
	go func() {
		if err := daemon.RunBridge(command.Context(), bridgeConfig); err != nil {
			logf("微信桥退出：%v", err)
		}
	}()
	return nil
}
