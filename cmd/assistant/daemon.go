package main

import (
	"bufio"
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	builtinagents "assistant/internal/agents"
	"assistant/internal/claudecfg"
	"assistant/internal/credentials"
	"assistant/internal/daemon"
	"assistant/internal/envref"
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
	// Name 非空时写入 channels 列表的命名实例（多微信账号）；缺省写匿名实例
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
		Short: "扫码登录微信 Bot，把凭据写入 config.json 的 channels 列表",
		Long: "按 openclaw-weixin（ilink）协议拉起扫码登录：终端展示二维码内容，\n" +
			"手机扫码确认后把 Bot token 写入 config.json 的 channels 列表；之后\n" +
			"assistant run 即可通过微信与 daemon 对话（channels 里有条目即启用）。\n" +
			"需要验证码时会提示输入手机上显示的验证码。\n" +
			"多微信账号用 --name <实例名>：凭据写入同名命名实例，会话键为\n" +
			"weixin/<实例名>，各实例互不串会话；缺省写匿名实例（会话键 weixin）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runWeixinLogin(command, *configFlag, loginOptions)
		},
	}
	loginCommand.Flags().StringVar(&loginOptions.BaseURL, "base-url", "",
		"ilink API 根地址（缺省 "+instances.DefaultWeixinBaseURL+"）")
	loginCommand.Flags().StringVar(&loginOptions.Name, "name", "",
		"写入 channels 列表的实例名（多微信账号用；缺省写匿名实例）")
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
			if file == nil {
				fmt.Fprintln(stdout, "未配置微信桥（先 assistant weixin login）")
				return nil
			}
			configs := weixinChannelViews(file)
			if len(configs) == 0 {
				fmt.Fprintln(stdout, "未配置微信桥（先 assistant weixin login）")
				return nil
			}
			for _, view := range configs {
				login := view.LoginUserID
				if login == "" {
					login = "-"
				}
				fmt.Fprintf(stdout, "key=%s enabled=%v base_url=%s login=%s bot_id=%s token=%s admins=%d\n",
					view.Key, view.Enabled, view.BaseURL, login, orDash(view.BotID), tokenState(view.BotToken), len(view.AdminUsers))
			}
			return nil
		},
	}
	command.AddCommand(loginCommand, statusCommand)
	return command
}

// weixinChannelView 是 weixin status 的展示条目（通道键 + 生效字段）。
type weixinChannelView struct {
	Key         string
	Enabled     bool
	BaseURL     string
	LoginUserID string
	BotID       string
	BotToken    string
	AdminUsers  []string
}

// weixinChannelViews 列出 channels 里的 weixin 实例（未启用的也列出，标注状态）。
func weixinChannelViews(file *instances.File) []weixinChannelView {
	var views []weixinChannelView
	for _, channel := range file.Channels {
		if channel.Type != instances.ChannelWeixin {
			continue
		}
		views = append(views, weixinChannelView{
			Key:         channel.Key(),
			Enabled:     channel.IsEnabled(),
			BaseURL:     channel.BaseURL,
			LoginUserID: channel.LoginUserID,
			BotID:       channel.BotID,
			BotToken:    channel.BotToken,
			AdminUsers:  channel.AdminUsers,
		})
	}
	return views
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
	// 缺省写匿名实例（Key()="weixin"，与旧版单实例会话键一致，老用户无感）
	entry := instances.Channel{Type: instances.ChannelWeixin}
	found := false
	for index := range file.Channels {
		if file.Channels[index].Type == instances.ChannelWeixin && strings.TrimSpace(file.Channels[index].Name) == "" {
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
	fmt.Fprintf(stdout, "登录成功：bot_id=%s login=%s，凭据已写入 %s 的 channels 实例 weixin（0600）\n",
		orDash(credentials.BotID), orDash(credentials.UserID), writePath)
	fmt.Fprintln(stdout, "启动对话：assistant run（channels 列表里有条目即启用）")
	return nil
}

// startDaemonServices 启动 daemon 模式的服务面：只读状态 API 与 runtime 引用的
// 对话通道（weixin/qq/telegram；gitea 通道由调度引擎接管）。API 先就绪——对话
// 会话的 daemon MCP 依赖端点文件自举发现。通道共享一个 Chat 实例：multi-user
// 由「通道 + 用户 → 会话」映射隔离，对话统一由 runtime 的主 agent 接待、子代理
// 按需委派。configPath 是解析后的配置路径：端点文件与会话环境都锚定它。
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

	resolvedPath, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil {
		// 静默跳过是最难排查的一种：明确说清通道没起以及为什么
		logf("对话通道未启用：config.json 里没有通道配置")
		return nil
	}
	runtime, err := file.ResolveRuntime(options.Runtime)
	if err != nil {
		return err
	}
	logRuntimeSummary(logf, resolvedPath, file, runtime, options)

	// 状态 API 监听：旗标 > runtime.api_listen（off/none 关闭）；端点文件锚定
	// 解析后的配置目录（--config 指向哪里，daemon.json 就落哪里）
	listen := firstNonEmpty(strings.TrimSpace(options.APIListen), runtime.ListenAddr())
	if listen != "" && !strings.EqualFold(listen, "none") && !strings.EqualFold(listen, "off") {
		if _, err := daemon.Serve(command.Context(), listen, resolvedPath, store, version, logf, stateStore); err != nil {
			return err
		}
	}

	// 主 agent + 子代理：启动期解析（配置问题立刻报错），凭据自检逐 provider
	// 实测（失败只告警，不拦启动）
	mainAgent, err := buildMainAgentRuntime(file, runtime, options)
	if err != nil {
		return err
	}
	subagents, err := buildSubagents(file, runtime, options)
	if err != nil {
		return err
	}
	remote, remoteSource := resolveSessionsRemote(file, resolvedPath)
	chat, err := daemon.NewChat(daemon.ChatConfig{
		MainAgent:  mainAgent,
		Subagents:  subagents,
		StateDir:   migrateRuntimeDir(filepath.Join(filepath.Dir(configPath), "chat"), runtime.ChatStateDir(), logf),
		SessionDir: migrateRuntimeDir(filepath.Join(filepath.Dir(configPath), "claude"), runtime.ClaudeConfigDir(), logf),
		Debug:      options.Debug,
		Remote:     remote,
		Log:        logf,
		// 会话内 assistant mcp daemon/sessions 靠它定位同一份配置与端点文件
		AssistantConfig: resolvedPath,
	})
	if err != nil {
		return err
	}
	logChatSummary(logf, mainAgent, subagents, chat, remote, remoteSource)
	checkChatCredentials(command, file, mainAgent, subagents, logf)

	// 通道：只启动 runtime 引用的通道；gitea 通道由调度引擎接管（本函数跳过）。
	referenced := map[string]bool{}
	for _, key := range runtime.Channels {
		referenced[key] = true
	}
	started := 0
	for index := range file.Channels {
		entry := file.Channels[index]
		if entry.Type == instances.ChannelGitea {
			continue
		}
		if len(runtime.Channels) > 0 && !referenced[entry.Key()] {
			logf("通道 %s 未被 runtime 引用，不启动（runtimes.<名>.channels 里加上它）", entry.Key())
			continue
		}
		if !entry.IsEnabled() {
			logf("通道 %s 已停用（enabled=false）", entry.Key())
			continue
		}
		if err := startChannelEntry(command, entry, options, chat, logf); err != nil {
			return err
		}
		started++
	}
	if len(runtime.Channels) > 0 && started == 0 {
		logf("runtime 未引用任何对话通道：只运行 gitea 通道（调度引擎）与状态 API")
	}
	return nil
}

// logRuntimeSummary 打印运行时装配结果与读写清单：一段集中说清 daemon 会读
// 哪些文件、写哪些目录（路径/用途/开关），让运行期的文件系统副作用可感知。
// 按 runtime 实际引用的通道裁剪：纯聊天 runtime 不列 repos/review/credentials。
func logRuntimeSummary(
	logf func(string, ...any),
	resolvedPath string,
	file *instances.File,
	runtime instances.Runtime,
	options *dispatcherOptions,
) {
	mainAgent := runtime.MainAgent
	if mainAgent == "" {
		mainAgent = instances.DefaultMainAgent
	}
	subagents := runtime.Subagents
	if subagents == nil {
		subagents = builtinagents.SubagentNames()
	}
	logf("runtime 装配：main agent = %s；子代理 = %s；通道 = %s",
		mainAgent, strings.Join(subagents, "、"), strings.Join(runtime.Channels, "、"))

	configDir := filepath.Dir(resolvedPath)
	gitea := runtimeGiteaChannels(file, runtime)
	logf("读写清单（写入落点可用 runtimes.<名> 的路径字段调整；off 项本运行关闭）：")
	logf("  读 config.json = %s", resolvedPath)
	dotEnv := filepath.Join(configDir, ".env")
	if _, err := os.Stat(dotEnv); err == nil {
		logf("  读 .env = %s（已载入，不覆盖已有环境变量；密钥引用 $VAR 由此解析）", dotEnv)
	} else {
		logf("  读 .env = 未找到（密钥引用 $VAR 用进程环境解析）")
	}
	if len(gitea) > 0 {
		credentialsPath, err := credentials.PathFor(resolvedPath)
		if err != nil {
			credentialsPath = "credentials.json"
		}
		logf("  读 credentials.json = %s（gitea 通道令牌兜底，只读）", credentialsPath)
	}
	if statePath := runtime.StatePath(); statePath != "" {
		logf("  写 状态库 = %s（SQLite WAL：内省 + 跨进程互斥）", statePath)
	} else {
		logf("  写 状态库 = off（state_file=\"off\"）")
	}
	listen := firstNonEmpty(strings.TrimSpace(options.APIListen), runtime.ListenAddr())
	apiOff := listen == "" || strings.EqualFold(listen, "none") || strings.EqualFold(listen, "off")
	if apiOff {
		logf("  写 状态 API = off（api_listen 或 --api-listen 关闭）")
	} else {
		endpoint, err := instances.DaemonEndpointPathFor(resolvedPath)
		if err != nil {
			endpoint = "daemon.json"
		}
		logf("  写 状态 API 端点 = %s（0600，退出时删除；崩溃残留由下次启动覆盖）@%s", endpoint, listen)
	}
	logf("  写 会话配置根 = %s（claude 转录 projects/ 与全局配置）", runtime.ClaudeConfigDir())
	logf("  写 对话状态 = %s（conversations/sessions.json + 每会话工作目录 chat-xxxxxxxx，首条消息懒建）", runtime.ChatStateDir())
	if len(gitea) > 0 {
		repoCount := 0
		for _, channel := range gitea {
			repoCount += len(channel.Repos)
		}
		logf("  写 受管克隆 = %s（gitea 通道按需 clone，%d 个仓库）", runtime.ReposRoot(), repoCount)
		logf("  写 评审工作区 = %s（PR/Issue worktree，按需）", runtime.ReviewRootDir())
		logf("  写 每仓库状态 = %s/<站点>/<owner>/<name>/{logs,dispatcher.lock}（%d 个 gitea 通道）",
			filepath.Dir(runtime.StatePath()), len(gitea))
	}
}

// runtimeGiteaChannels 返回 runtime 引用的 gitea 通道（按 runtime.Channels 过滤；
// 未引用任何时回退空——纯聊天 runtime 不产生调度写入）。
func runtimeGiteaChannels(file *instances.File, runtime instances.Runtime) []instances.Channel {
	referenced := map[string]bool{}
	for _, key := range runtime.Channels {
		referenced[key] = true
	}
	var channels []instances.Channel
	for _, channel := range file.Channels {
		if channel.Type != instances.ChannelGitea {
			continue
		}
		if len(runtime.Channels) > 0 && !referenced[channel.Key()] {
			continue
		}
		channels = append(channels, channel)
	}
	return channels
}

// migrateRuntimeDir 把旧运行目录整体搬到 runtime 树下：同盘 rename 一次性迁移，
// 失败（跨盘/占用）回退旧路径继续用，不让目录搬迁拦住 daemon 启动。
func migrateRuntimeDir(oldDir, newDir string, logf func(string, ...any)) string {
	oldDir = filepath.Clean(oldDir)
	newDir = filepath.Clean(newDir)
	if oldDir == newDir {
		return newDir
	}
	if _, err := os.Stat(oldDir); err != nil {
		return newDir // 旧目录不存在：直接用新路径
	}
	if _, err := os.Stat(newDir); err == nil {
		// 新目录已在用（迁移过或用户自配）：旧目录留给用户自行处理
		return newDir
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		logf("运行目录迁移失败（%s → %s）：%v；继续用旧路径", oldDir, newDir, err)
		return oldDir
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		logf("运行目录迁移失败（%s → %s）：%v；继续用旧路径", oldDir, newDir, err)
		return oldDir
	}
	logf("运行目录已迁移：%s → %s", oldDir, newDir)
	return newDir
}

// startChannelEntry 启动 channels 列表里的一条通道实例（runtime 引用即启动）。
func startChannelEntry(
	command *cobra.Command,
	entry instances.Channel,
	options *dispatcherOptions,
	chat *daemon.Chat,
	logf func(string, ...any),
) error {
	key := entry.Key()
	// 密钥的 $VAR/${VAR} 引用在消费点展开（File 里的原始定义不动）；未定义
	// 变量在这里拦下，不让字面量发往平台。
	if secretField, secretValue := entry.SecretField(); secretField != "" {
		expanded, err := envref.Expand(secretValue, envref.Options{
			Field: fmt.Sprintf("channels[%s].%s", key, secretField),
		})
		if err != nil {
			return fmt.Errorf("通道 %s 启动失败：%w", key, err)
		}
		switch secretField {
		case "app_secret":
			entry.AppSecret = expanded
		case "bot_token":
			entry.BotToken = expanded
		case "token":
			entry.Token = expanded
		}
	}
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
		startChannel(command, channel, chat, options.Debug, logf)
	case instances.ChannelQQ:
		channel := daemon.NewQQChannel(daemon.QQChannelConfig{
			AppID:      entry.AppID,
			AppSecret:  entry.AppSecret,
			APIBaseURL: entry.APIBaseURL,
			Sandbox:    entry.Sandbox,
			AdminUsers: entry.AdminUsers,
			SplitLimit: entry.SplitLimit,
		}, key, options.Debug, logf)
		startChannel(command, channel, chat, options.Debug, logf)
	case instances.ChannelTelegram:
		channel := daemon.NewTelegramChannel(daemon.TelegramChannelConfig{
			Name:       key,
			BotToken:   entry.BotToken,
			APIBaseURL: entry.APIBaseURL,
			AdminUsers: entry.AdminUsers,
			SplitLimit: entry.SplitLimit,
		}, logf)
		startChannel(command, channel, chat, options.Debug, logf)
	case instances.ChannelGitea:
		return fmt.Errorf("gitea 通道不经过对话桥启动（由调度引擎接管）：%s", key)
	default:
		return fmt.Errorf("channels: 未知平台类型 %q（应为 weixin/qq/telegram/gitea）", entry.Type)
	}
	logf("通道 %s 已启动（白名单 %d 人）", key, len(entry.AdminUsers))
	return nil
}

// startChannel 是通道启动的公共包装：通用桥配置 + 后台协程。
func startChannel(
	command *cobra.Command,
	channel daemon.Channel,
	chat *daemon.Chat,
	debug bool,
	logf func(string, ...any),
) {
	config := daemon.ChannelConfig{Chat: chat, Log: logf, Debug: debug}
	go func() {
		if err := daemon.RunChannel(command.Context(), channel, config); err != nil {
			logf("通道 %s 退出：%v", channel.Name(), err)
		}
	}()
}

// buildMainAgentRuntime 解析 runtime 的主 agent 生效运行时：内置 main 预设打底，
// config.json 的 agents[main_agent] 同名覆盖；provider 链 agent > runtime.provider
// > 全局默认，bare 探测与超时逐项落实。
func buildMainAgentRuntime(
	file *instances.File,
	runtime instances.Runtime,
	options *dispatcherOptions,
) (daemon.AgentRuntime, error) {
	name := runtime.MainAgent
	if name == "" {
		name = instances.DefaultMainAgent
	}
	definition, builtinOK := builtinagents.Lookup(name)
	if !builtinOK {
		if _, userOK := file.Agents[name]; !userOK {
			return daemon.AgentRuntime{}, fmt.Errorf("主 agent %q 未定义（用户 agents：%s；内置：%s）",
				name, file.AgentNames(), strings.Join(builtinagents.Names(), "、"))
		}
	}
	if user, ok := file.Agents[name]; ok {
		definition.Description = cmp.Or(user.Description, definition.Description)
		definition.SystemPrompt = cmp.Or(user.SystemPrompt, definition.SystemPrompt)
		definition.Model = cmp.Or(user.Model, definition.Model)
		expanded, err := envref.ExpandMCPEnv(user.MCP, envref.Options{
			Field: fmt.Sprintf("agents[%s].mcp", name),
		})
		if err != nil {
			return daemon.AgentRuntime{}, err
		}
		definition.MCP, _ = expanded.(map[string]any)
	}
	return agentRuntimeFromDefinition(file, definition, runtime, options)
}

// buildSubagents 解析 runtime 的子代理定义：缺省全部内置子代理；显式清单逐个
// 解析（用户 agents 同名覆盖内置预设）。子代理的 MCP 并入会话面、模型进
// --agents JSON；provider 仍走主 agent 的（委派执行不换供应商）。
func buildSubagents(
	file *instances.File,
	runtime instances.Runtime,
	options *dispatcherOptions,
) ([]daemon.SubagentDefinition, error) {
	definitions := make([]builtinagents.Definition, 0, len(runtime.Subagents))
	if runtime.Subagents == nil {
		definitions = builtinagents.Subagents()
	} else {
		for _, name := range runtime.Subagents {
			definition, ok := builtinagents.Lookup(name)
			if !ok {
				definition = builtinagents.Definition{Name: name}
			}
			if user, ok := file.Agents[name]; ok {
				definition.Description = cmp.Or(user.Description, definition.Description)
				definition.SystemPrompt = cmp.Or(user.SystemPrompt, definition.SystemPrompt)
				definition.Model = cmp.Or(user.Model, definition.Model)
				expanded, err := envref.ExpandMCPEnv(user.MCP, envref.Options{
					Field: fmt.Sprintf("agents[%s].mcp", name),
				})
				if err != nil {
					return nil, err
				}
				definition.MCP, _ = expanded.(map[string]any)
			}
			definitions = append(definitions, definition)
		}
	}
	subagents := make([]daemon.SubagentDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if strings.TrimSpace(definition.Description) == "" {
			definition.Description = truncateDescription(definition.SystemPrompt)
		}
		subagents = append(subagents, daemon.SubagentDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			Prompt:      definition.SystemPrompt,
			Model:       definition.Model,
			MCP:         definition.MCP,
		})
	}
	return subagents, nil
}

// agentRuntimeFromDefinition 把 agent 定义落成生效运行时：provider 链解析、
// MCP 合并、claude_bin 探测与超时。
func agentRuntimeFromDefinition(
	file *instances.File,
	definition builtinagents.Definition,
	runtime instances.Runtime,
	options *dispatcherOptions,
) (daemon.AgentRuntime, error) {
	providerName := file.AgentProviderName(definition.Name)
	if definition.Name != "" && file.Agents[definition.Name].Provider == "" {
		// 内置预设/未写 provider 的 agent：runtime.provider 优先于全局默认
		if runtime.Provider != "" {
			providerName = runtime.Provider
		}
	}
	overrides, err := file.EffectiveOverrides(providerName)
	if err != nil {
		return daemon.AgentRuntime{}, fmt.Errorf("agents[%s] provider %s: %w", definition.Name, providerName, err)
	}
	if len(definition.MCP) > 0 {
		if overrides.MCP == nil {
			overrides.MCP = map[string]any{}
		}
		maps.Copy(overrides.MCP, definition.MCP)
	}
	claudeBin := firstNonEmpty(file.Agents[definition.Name].ClaudeBin, options.ClaudeBin, "claude")
	return daemon.AgentRuntime{
		Name:         definition.Name,
		Provider:     overrides,
		ProviderName: providerName,
		Model:        definition.Model,
		SystemPrompt: definition.SystemPrompt,
		ClaudeBin:    claudeBin,
		Bare:         claudecfg.SupportsBare(claudeBin),
		Timeout:      time.Duration(file.Agents[definition.Name].SessionTimeoutMS) * time.Millisecond,
	}, nil
}

// truncateDescription 取提示词首行做描述兜底（委派质量靠 description）。
func truncateDescription(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if head, _, found := strings.Cut(prompt, "\n"); found {
		prompt = head
	}
	runes := []rune(prompt)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	if prompt == "" {
		return "专项任务子代理"
	}
	return prompt
}

// logChatSummary 打印对话会话的前提：claude 与最小模式、主 agent 与子代理、
// 记录库去向（来源标明 config/env/sidecar）。一次说清，别让「为什么没生效」靠猜。
func logChatSummary(
	logf func(string, ...any),
	mainAgent daemon.AgentRuntime,
	subagents []daemon.SubagentDefinition,
	chat *daemon.Chat,
	remote sessionstore.RemoteConfig,
	remoteSource string,
) {
	bareLabel := "关（claude 不支持 --bare 或未探测到）"
	if mainAgent.Bare {
		bareLabel = "开（不读 hooks/插件/CLAUDE.md，只用显式 settings 与自举 MCP）"
	}
	logf("对话会话：")
	logf("  main agent = %s（provider %s，model %s）", agentLabel(mainAgent.Name),
		displayName(mainAgent.ProviderName, nil), orDash(mainAgent.Model))
	logf("  子代理     = %s（注入 --agents，主模型按 description 委派）",
		strings.Join(subagentNames(subagents), "、"))
	logf("  claude     = %s", mainAgent.ClaudeBin)
	logf("  bare       = %s", bareLabel)
	// 配置根/会话目录已在启动读写清单里列出，这里只留操作者要用的续聊提示
	logf("  续聊       = cd <会话目录> && claude --continue（目录见读写清单的对话状态行）")
	if remote.URL != "" {
		logf("  记录库     = %s（来源 %s；每轮结束归档该会话，可用 sessions MCP 回查）", remote.URL, remoteSource)
	} else {
		logf("  记录库     = 未配置（config.json 加 sessions.remote，或 assistant serve + session push 可远端留存记录）")
	}
}

// resolveSessionsRemote 解析远端记录库连接：config.json 的 sessions 节优先
// （token 经 $VAR/${VAR} 展开），环境变量与 sidecar 文件（sessions-remote.json/
// serve.json）兜底。返回连接配置与来源标签。
func resolveSessionsRemote(file *instances.File, configPath string) (sessionstore.RemoteConfig, string) {
	if file.Sessions != nil && file.Sessions.Remote != nil && strings.TrimSpace(file.Sessions.Remote.URL) != "" {
		config := sessionstore.RemoteConfig{URL: strings.TrimSpace(file.Sessions.Remote.URL)}
		if file.Sessions.Remote.Token != "" {
			expanded, err := envref.Expand(file.Sessions.Remote.Token, envref.Options{Field: "sessions.remote.token"})
			if err != nil {
				// 装载期 validateSecretRefs 已拦截未定义引用；这里兜底保留原值
				expanded = file.Sessions.Remote.Token
			}
			config.Token = expanded
		}
		return config, "config"
	}
	config := sessionstore.ResolveRemote(filepath.Dir(configPath))
	if config.URL != "" {
		return config, "env/sidecar"
	}
	return config, "未配置"
}

// subagentNames 返回子代理名（保持 runtime 声明顺序）。
func subagentNames(subagents []daemon.SubagentDefinition) []string {
	names := make([]string, 0, len(subagents))
	for _, subagent := range subagents {
		names = append(names, subagent.Name)
	}
	return names
}

// agentLabel 是日志里的 agent 展示名。
func agentLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "内置 main"
	}
	return name
}

// checkChatCredentials 对主 agent 的 provider 做最小请求实测：密钥/端点不配套
// 会让每条消息静默重试几分钟，启动时就说清楚。失败只告警。
func checkChatCredentials(
	command *cobra.Command,
	file *instances.File,
	mainAgent daemon.AgentRuntime,
	subagents []daemon.SubagentDefinition,
	logf func(string, ...any),
) {
	warnf := func(format string, arguments ...any) {
		fmt.Fprintf(command.ErrOrStderr(), "警告："+format+"\n", arguments...)
	}
	source := claudecfg.CredentialSource(mainAgent.Provider)
	if source == "" {
		warnf("主 agent %s 没有可用的 AI 凭据：对话会认证失败；%s",
			agentLabel(mainAgent.Name), claudecfg.MissingCredentialHint)
		return
	}
	check := provider.CheckCredential(command.Context(), mainAgent.Provider)
	if check.OK() {
		logf("主 agent AI 凭据：provider = %s，来源 = %s，自检 = %s",
			displayName(mainAgent.ProviderName, file), source, check.Describe())
		return
	}
	warnf("主 agent 凭据自检失败：%s", check.Describe())
	warnf("%s", provider.CredentialHint)
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
