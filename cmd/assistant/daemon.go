package main

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	builtinagents "assistant/internal/agents"
	"assistant/internal/claudecfg"
	"assistant/internal/daemon"
	"assistant/internal/instances"
	"assistant/internal/provider"
	"assistant/internal/sessionstore"
	"assistant/internal/statestore"
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
	// Name 非空时写入 channels 列表的命名实例（多微信账号）；缺省写旧版 weixin 节
	Name string
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
			"需要验证码时会提示输入手机上显示的验证码。\n" +
			"多微信账号用 --name <实例名>：凭据写入 channels 列表的命名实例，\n" +
			"会话键为 weixin/<实例名>，各实例互不串会话。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runWeixinLogin(command, *configFlag, loginOptions)
		},
	}
	loginCommand.Flags().StringVar(&loginOptions.BaseURL, "base-url", "",
		"ilink API 根地址（缺省 "+instances.DefaultWeixinBaseURL+"）")
	loginCommand.Flags().StringVar(&loginOptions.Name, "name", "",
		"写入 channels 列表的实例名（多微信账号用；缺省写旧版 weixin 节）")
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
	if name := strings.TrimSpace(options.Name); name != "" {
		// channels 列表的命名实例：找同名实例更新凭据，没有就追加
		entry := instances.Channel{Type: instances.ChannelWeixin, Name: name}
		found := false
		for index := range file.Channels {
			if file.Channels[index].Type == instances.ChannelWeixin && file.Channels[index].Name == name {
				entry = file.Channels[index]
				found = true
				break
			}
		}
		entry.BaseURL = credentials.BaseURL
		entry.BotToken = credentials.BotToken
		entry.BotID = credentials.BotID
		entry.LoginUserID = credentials.UserID
		if !found {
			file.Channels = append(file.Channels, entry)
		}
		file.Normalize()
		if err := file.Validate(); err != nil {
			return err
		}
		if err := instances.Save(writePath, file); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "登录成功：bot_id=%s login=%s，凭据已写入 %s 的 channels 实例 weixin/%s（0600）\n",
			orDash(credentials.BotID), orDash(credentials.UserID), writePath, name)
		fmt.Fprintln(stdout, "启动对话：assistant run（channels 列表里有条目即启用）")
		return nil
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
// 127.0.0.1:8770）与启用的对话通道（weixin/qq）。API 先就绪——对话会话的
// daemon MCP 依赖端点文件自举发现。通道共享一个 Chat 实例：multi-user 由
// 「通道 + 用户 → 会话」映射隔离，multi-agent 由命名 agent 池提供。
func startDaemonServices(
	command *cobra.Command,
	configPath string,
	options *dispatcherOptions,
	store *daemon.Store,
	stateStore *statestore.Store,
) error {
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), "[daemon %s] %s\n",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fmt.Sprintf(format, arguments...))
	}
	listen := strings.TrimSpace(options.APIListen)
	if listen != "" && !strings.EqualFold(listen, "none") && !strings.EqualFold(listen, "off") {
		if _, err := daemon.Serve(command.Context(), listen, store, version, logf, stateStore); err != nil {
			return err
		}
	}

	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil {
		if options.Weixin || options.QQ {
			return fmt.Errorf("--weixin/--qq 需要 config.json 配置：先 assistant config init（或 weixin login）")
		}
		// 静默跳过是最难排查的一种：明确说清通道没起以及为什么
		logf("对话通道未启用：config.json 里没有 weixin/qq 配置")
		return nil
	}

	// agent 池：每个 agent 独立 provider 链与执行参数；启动期解析（配置问题
	// 立刻报错），凭据自检逐 provider 实测（失败只告警，不拦启动）
	agents, err := buildAgentRuntimes(file, options)
	if err != nil {
		return err
	}

	// 内置缺省 agent 的取值沿用 weixin 节的对话参数（weixin.provider > 全局默认），
	// 未配置 agent 池时行为与旧版本一致
	weixinConfig := file.Weixin
	providerName := file.WeixinProviderName()
	providerOverrides, err := file.EffectiveOverrides(providerName)
	if err != nil {
		return fmt.Errorf("对话会话 provider %s: %w", providerName, err)
	}
	claudeBin := firstNonEmpty(options.ClaudeBin, "claude")
	var model string
	var timeout time.Duration
	if weixinConfig != nil {
		claudeBin = firstNonEmpty(weixinConfig.ClaudeBin, options.ClaudeBin, "claude")
		model = weixinConfig.Model
		timeout = time.Duration(weixinConfig.SessionTimeoutMS) * time.Millisecond
	}
	// 对话会话走 claude 的最小模式（--bare）：上下文本就全由显式参数给出，最小
	// 模式顺带甩掉 hooks、插件同步、CLAUDE.md 自动发现与记忆。仅在 claude 真的
	// 支持该参数时打开，不支持就退回普通模式，不让对话起不来。
	bare := claudecfg.SupportsBare(claudeBin)
	remote := sessionstore.ResolveRemote(filepath.Dir(configPath))
	chat, err := daemon.NewChat(daemon.ChatConfig{
		ClaudeBin:    claudeBin,
		Bare:         bare,
		Debug:        options.Debug,
		Remote:       remote,
		Model:        firstNonEmpty(model, options.Model),
		Provider:     providerOverrides,
		ProviderName: providerName,
		Timeout:      timeout,
		Agents:       agents,
		DefaultAgent: file.DefaultAgent,
		Log:          logf,
	})
	if err != nil {
		return err
	}
	logChatSummary(logf, file, chat, claudeBin, bare, remote, agents)
	checkChatCredentials(command, file, providerName, providerOverrides, agents, logf)

	// 通道：channels 列表（多实例，推荐）逐条启动；旧版单实例 weixin/qq 块
	// 继续按 enabled/--旗标语义工作
	for index := range file.Channels {
		if err := startChannelEntry(command, file.Channels[index], options, chat, logf); err != nil {
			return err
		}
	}
	if err := startWeixinChannel(command, file, weixinConfig, options, chat, logf); err != nil {
		return err
	}
	if err := startQQChannel(command, file, options, chat, logf); err != nil {
		return err
	}
	return nil
}

// startChannelEntry 启动 channels 列表里的一条通道实例（列表里有条目即启用）。
func startChannelEntry(
	command *cobra.Command,
	entry instances.Channel,
	options *dispatcherOptions,
	chat *daemon.Chat,
	logf func(string, ...any),
) error {
	key := entry.Key()
	switch entry.Type {
	case instances.ChannelWeixin:
		channel := daemon.NewWeixinChannel(daemon.WeixinChannelConfig{
			Name: key,
			Weixin: weixin.Config{
				BaseURL:        entry.BaseURL,
				BotToken:       entry.BotToken,
				BotAgent:       entry.BotAgent,
				ChannelVersion: firstNonEmpty(entry.ChannelVersion, version),
				RouteTag:       entry.RouteTag,
			},
			AdminUsers:  entry.AdminUsers,
			LoginUserID: entry.LoginUserID,
			SplitLimit:  entry.SplitLimit,
		}, logf)
		startChannel(command, channel, chat, entry.Agent, options.Debug, logf)
	case instances.ChannelQQ:
		channel := daemon.NewQQChannel(instances.QQ{
			Enabled:    true,
			AppID:      entry.AppID,
			AppSecret:  entry.AppSecret,
			APIBaseURL: entry.APIBaseURL,
			Sandbox:    entry.Sandbox,
			AdminUsers: entry.AdminUsers,
			Agent:      entry.Agent,
			SplitLimit: entry.SplitLimit,
		}, key, options.Debug, logf)
		startChannel(command, channel, chat, entry.Agent, options.Debug, logf)
	case instances.ChannelTelegram:
		channel := daemon.NewTelegramChannel(daemon.TelegramChannelConfig{
			Name:       key,
			BotToken:   entry.BotToken,
			APIBaseURL: entry.APIBaseURL,
			AdminUsers: entry.AdminUsers,
			SplitLimit: entry.SplitLimit,
		}, logf)
		startChannel(command, channel, chat, entry.Agent, options.Debug, logf)
	default:
		return fmt.Errorf("channels: 未知平台类型 %q（应为 weixin/qq/telegram）", entry.Type)
	}
	logf("通道 %s 已启动（白名单 %d 人）", key, len(entry.AdminUsers))
	return nil
}

// startChannel 是通道启动的公共包装：通用桥配置 + 后台协程。
func startChannel(
	command *cobra.Command,
	channel daemon.Channel,
	chat *daemon.Chat,
	agent string,
	debug bool,
	logf func(string, ...any),
) {
	config := daemon.ChannelConfig{Chat: chat, DefaultAgent: agent, Log: logf, Debug: debug}
	go func() {
		if err := daemon.RunChannel(command.Context(), channel, config); err != nil {
			logf("通道 %s 退出：%v", channel.Name(), err)
		}
	}()
}

// buildAgentRuntimes 组装生效的 agent 池：内嵌预设（internal/agents，随二进制
// 分发，system prompt/MCP 开箱即用）打底，config.json 的 agents 同名覆盖其上、
// 其余为新增。provider 链解析与 bare 探测逐 agent 进行。
func buildAgentRuntimes(file *instances.File, options *dispatcherOptions) (map[string]daemon.AgentRuntime, error) {
	agents := make(map[string]daemon.AgentRuntime, len(file.Agents)+len(builtinagents.List()))
	defaultClaudeBin := firstNonEmpty(options.ClaudeBin, "claude")
	defaultBare := claudecfg.SupportsBare(defaultClaudeBin)
	for _, definition := range builtinagents.List() {
		agents[definition.Name] = daemon.AgentRuntime{
			Name:         definition.Name,
			Model:        definition.Model,
			SystemPrompt: definition.SystemPrompt,
			ClaudeBin:    defaultClaudeBin,
			Bare:         defaultBare,
		}
	}
	for _, name := range file.AgentNames() {
		definition := file.Agents[name]
		providerName := file.AgentProviderName(name)
		overrides, err := file.EffectiveOverrides(providerName)
		if err != nil {
			return nil, fmt.Errorf("agents[%s] provider %s: %w", name, providerName, err)
		}
		claudeBin := firstNonEmpty(definition.ClaudeBin, options.ClaudeBin, "claude")
		// agent 专属 MCP 合并进 provider 覆盖（同名 server 覆盖自举与供应商的）
		if len(definition.MCP) > 0 {
			if overrides.MCP == nil {
				overrides.MCP = map[string]any{}
			}
			maps.Copy(overrides.MCP, definition.MCP)
		}
		agents[name] = daemon.AgentRuntime{
			Name:         name,
			Provider:     overrides,
			ProviderName: providerName,
			Model:        definition.Model,
			SystemPrompt: definition.SystemPrompt,
			ClaudeBin:    claudeBin,
			Bare:         claudecfg.SupportsBare(claudeBin),
			Timeout:      time.Duration(definition.SessionTimeoutMS) * time.Millisecond,
		}
	}
	return agents, nil
}

// logChatSummary 打印对话会话的前提：claude 与最小模式、目录布局、agent 池、
// 记录库。一次说清，别让「为什么没生效」靠猜。
func logChatSummary(
	logf func(string, ...any),
	file *instances.File,
	chat *daemon.Chat,
	claudeBin string,
	bare bool,
	remote sessionstore.RemoteConfig,
	agents map[string]daemon.AgentRuntime,
) {
	bareLabel := "关（claude 不支持 --bare 或未探测到）"
	if bare {
		bareLabel = "开（不读 hooks/插件/CLAUDE.md，只用显式 settings 与自举 MCP）"
	}
	logf("对话会话：")
	logf("  claude   = %s", claudeBin)
	logf("  bare     = %s", bareLabel)
	logf("  配置根   = %s", chat.SessionDir())
	logf("  会话目录 = %s（每个会话一个稳定工作目录 <chat-xxxxxxxx>，含 session.json）", chat.StateDir())
	logf("  续聊     = cd <会话目录> && claude --continue")
	if len(agents) == 0 {
		logf("  agent 池 = 空（所有会话用内置缺省；/agent 不可用）")
	} else {
		defaultAgent := file.DefaultAgent
		if defaultAgent == "" {
			defaultAgent = "内置缺省"
		}
		logf("  agent 池 = %s（默认 %s；带 * 为用户定义，其余为内置预设；聊天里 /agent 可切换）",
			agentPoolSummary(file, agents), defaultAgent)
	}
	if remote.URL != "" {
		logf("  记录库   = %s（每轮结束归档该会话，可用 sessions MCP 回查）", remote.URL)
	} else {
		logf("  记录库   = 未配置（assistant serve + session push 可远端留存记录）")
	}
}

// agentPoolSummary 是 agent 池的日志展示：名字排序，用户定义的带 * 标记。
func agentPoolSummary(file *instances.File, agents map[string]daemon.AgentRuntime) string {
	names := slices.Sorted(maps.Keys(agents))
	labeled := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := file.Agents[name]; ok {
			labeled = append(labeled, name+"*")
			continue
		}
		labeled = append(labeled, name)
	}
	return strings.Join(labeled, "、")
}

// checkChatCredentials 对内置缺省与每个 agent 的 provider 做最小请求实测：
// 密钥/端点不配套会让每条消息静默重试几分钟，启动时就说清楚。失败只告警。
func checkChatCredentials(
	command *cobra.Command,
	file *instances.File,
	providerName string,
	providerOverrides claudecfg.Overrides,
	agents map[string]daemon.AgentRuntime,
	logf func(string, ...any),
) {
	warnf := func(format string, arguments ...any) {
		fmt.Fprintf(command.ErrOrStderr(), "警告："+format+"\n", arguments...)
	}
	checked := map[string]bool{}
	verify := func(label, name string, overrides claudecfg.Overrides) {
		if name == "" || checked[name] {
			return
		}
		checked[name] = true
		source := claudecfg.CredentialSource(overrides)
		if source == "" {
			warnf("%s 没有可用的 AI 凭据：对话会认证失败；%s", label, claudecfg.MissingCredentialHint)
			return
		}
		check := provider.CheckCredential(command.Context(), overrides)
		if check.OK() {
			logf("%s AI 凭据：provider = %s，来源 = %s，自检 = %s", label, displayName(name, file), source, check.Describe())
			return
		}
		warnf("%s 凭据自检失败：%s", label, check.Describe())
		warnf("%s", provider.CredentialHint)
	}
	verify("内置缺省", providerName, providerOverrides)
	for _, agent := range agents {
		verify("agent "+agent.Name, agent.ProviderName, agent.Provider)
	}
}

// displayName 是 provider 名的日志展示（内置预设加标注，缺省名可读化）。
func displayName(name string, file *instances.File) string {
	if name == "" {
		return "内置缺省"
	}
	if provider.HasPreset(name) {
		return name + " 内置预设"
	}
	return name
}

// startWeixinChannel 按配置与 --weixin 旗标启动微信通道。
func startWeixinChannel(
	command *cobra.Command,
	file *instances.File,
	weixinConfig *instances.Weixin,
	options *dispatcherOptions,
	chat *daemon.Chat,
	logf func(string, ...any),
) error {
	if weixinConfig == nil {
		if options.Weixin {
			return fmt.Errorf("--weixin 需要 config.json 的 weixin 配置：先 assistant weixin login")
		}
		logf("通道 weixin 未启用：config.json 里没有 weixin 配置（需要时先 `assistant weixin login` 扫码）")
		return nil
	}
	if !options.Weixin && !weixinConfig.Enabled {
		logf("通道 weixin 未启用：weixin.enabled=false（`assistant weixin login` 会置为 true；--weixin 可强制开启）")
		return nil
	}
	if strings.TrimSpace(weixinConfig.BotToken) == "" {
		return fmt.Errorf("weixin.bot_token 未配置：先 assistant weixin login")
	}
	channel := daemon.NewWeixinChannel(daemon.WeixinChannelConfig{
		Weixin: weixin.Config{
			BaseURL:        weixinConfig.BaseURL,
			BotToken:       weixinConfig.BotToken,
			BotAgent:       weixinConfig.BotAgent,
			ChannelVersion: firstNonEmpty(weixinConfig.ChannelVersion, version),
			RouteTag:       weixinConfig.RouteTag,
		},
		AdminUsers:  weixinConfig.AdminUsers,
		LoginUserID: weixinConfig.LoginUserID,
	}, logf)
	channelConfig := daemon.ChannelConfig{
		Chat:         chat,
		DefaultAgent: weixinConfig.Agent,
		Log:          logf,
		Debug:        options.Debug,
	}
	go func() {
		if err := daemon.RunChannel(command.Context(), channel, channelConfig); err != nil {
			logf("通道 weixin 退出：%v", err)
		}
	}()
	logf("通道 weixin 已启动（白名单 %d 人）", len(weixinConfig.AdminUsers))
	return nil
}

// startQQChannel 按配置与 --qq 旗标启动 QQ 通道。
func startQQChannel(
	command *cobra.Command,
	file *instances.File,
	options *dispatcherOptions,
	chat *daemon.Chat,
	logf func(string, ...any),
) error {
	qqConfig := file.QQ
	if qqConfig == nil {
		if options.QQ {
			return fmt.Errorf("--qq 需要 config.json 的 qq 配置：把 q.qq.com 开放平台的 app_id/app_secret 写入 qq 节")
		}
		logf("通道 qq 未启用：config.json 里没有 qq 配置（需要时在 q.qq.com 创建机器人后填入）")
		return nil
	}
	if !options.QQ && !qqConfig.Enabled {
		logf("通道 qq 未启用：qq.enabled=false（--qq 可强制开启）")
		return nil
	}
	if qqConfig.AppID == "" || qqConfig.AppSecret == "" {
		return fmt.Errorf("qq.app_id/qq.app_secret 未配置：在 q.qq.com 开放平台创建机器人后填入")
	}
	channel := daemon.NewQQChannel(*qqConfig, "qq", options.Debug, logf)
	channelConfig := daemon.ChannelConfig{
		Chat:         chat,
		DefaultAgent: qqConfig.Agent,
		Log:          logf,
		Debug:        options.Debug,
	}
	go func() {
		if err := daemon.RunChannel(command.Context(), channel, channelConfig); err != nil {
			logf("通道 qq 退出：%v", err)
		}
	}()
	logf("通道 qq 已启动（白名单 %d 人，api=%s）", len(qqConfig.AdminUsers), qqConfig.APIBaseURL)
	return nil
}
