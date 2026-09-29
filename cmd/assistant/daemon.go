package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	builtinagents "github.com/Cosmic-Developers-Union/assistant/internal/agents"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/daemon"
	"github.com/Cosmic-Developers-Union/assistant/internal/envref"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/qq"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/telegram"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/weixin"
	"github.com/Cosmic-Developers-Union/assistant/internal/provider"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"
	"github.com/Cosmic-Developers-Union/assistant/internal/statestore"

	"github.com/spf13/cobra"
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

// orDash / tokenState 是展示用的占位助手：诊断输出里空值统一打成 - 或 none，
// 免得操作者分不清「没配」和「配成空串」。绝不能回显密钥本身。
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

func endpointPathHint() string {
	path, err := instances.DaemonEndpointPath()
	if err != nil {
		return "daemon.json"
	}
	return path
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
	storage, storageSource := resolveSessionsStorage(file)
	sessionDir := migrateRuntimeDir(filepath.Join(filepath.Dir(configPath), "claude"), runtime.ClaudeConfigDir(), logf)
	stateDir := migrateRuntimeDir(filepath.Join(filepath.Dir(configPath), "chat"), runtime.ChatStateDir(), logf)
	archive, archiveErr := buildSessionArchive(command.Context(), runtime, storage, sessionDir, stateDir, logf)
	if archiveErr != nil {
		logf("⚠ 会话归档未启用：%v", archiveErr)
	}
	chat, err := daemon.NewChat(daemon.ChatConfig{
		MainAgent:  mainAgent,
		Subagents:  subagents,
		StateDir:   stateDir,
		SessionDir: sessionDir,
		Debug:      options.Debug,
		Archive:    archive,
		Log:        logf,
		// 会话内 assistant mcp daemon/sessions 靠它定位同一份配置与端点文件
		AssistantConfig: resolvedPath,
	})
	if err != nil {
		return err
	}
	logChatSummary(logf, mainAgent, subagents, chat, archive, storage, storageSource)
	checkChatCredentials(command, file, mainAgent, subagents, logf)
	// 后台巡检：立即跑一次再按拍全量归档，捞每轮钩子漏掉的评审/分诊会话。
	startSessionArchiveLoop(command.Context(), archive, logf)

	// 通道：只启动 runtime 引用的通道；gitea 通道由调度引擎接管（本函数跳过）。
	referenced := map[string]bool{}
	for _, key := range runtime.Channels {
		referenced[key] = true
	}
	credentialStore, err := loadCredentialStoreQuietly()
	if err != nil {
		// 读不到凭据库不该让整个 runtime 起不来：缺凭据的通道各自报缺，其余
		// 通道照常服务。真正要命的是配置写错，不是凭据库暂时不可读。
		logf("⚠ 读取凭据库失败（%v）：对话通道将回退到 config.json 内联凭据", err)
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
		// 单条通道起不来（缺凭据、端点写错）不该拖垮其余通道与调度引擎：
		// 响亮记下来并跳过这条，让操作者能一眼看到是哪一条、为什么。
		if err := startChannelEntry(command, entry, options, chat, credentialStore, logf); err != nil {
			logf("⚠ 通道 %s 未启动：%v", entry.Key(), err)
			continue
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
	referenced := runtimeChannelFilter(runtime)
	chatChannels := 0
	for _, channel := range file.Channels {
		if channel.Type != instances.ChannelGitea && channel.IsEnabled() &&
			(len(runtime.Channels) == 0 || referenced[channel.Key()]) {
			chatChannels++
		}
	}
	logf("读写清单（写入落点可用 runtimes.<名> 的路径字段调整；off 项本运行关闭）：")
	logf("  读 config.json = %s", resolvedPath)
	dotEnv := filepath.Join(configDir, ".env")
	if _, err := os.Stat(dotEnv); err == nil {
		logf("  读 .env = %s（已载入，不覆盖已有环境变量；凭据引用 $VAR 由此解析）", dotEnv)
	} else {
		logf("  读 .env = 未找到（凭据引用 $VAR 用进程环境解析）")
	}
	// 通道密钥（weixin/qq/telegram 按通道键）与 gitea 用途令牌都落在凭据库：
	// 只要 runtime 引用了任何一条通道，这次启动就会读它。
	if len(gitea) > 0 || chatChannels > 0 {
		credentialsPath, err := credentials.Path()
		if err != nil {
			credentialsPath = "credentials.json"
		}
		logf("  读 credentials.json = %s（对话通道密钥 + gitea 通道令牌兜底，只读；平台标准配置目录）", credentialsPath)
		// 旧版本（28ca459 前）把凭据放在 config.json 同目录：发现旧落点就提示
		// 迁移，不自动搬——凭据位置的变化必须让操作者看见。
		if legacy := filepath.Join(configDir, "credentials.json"); legacy != credentialsPath {
			if _, err := os.Stat(legacy); err == nil {
				logf("  ⚠ 发现旧落点凭据 %s：请迁移到 %s（mv 后重启；凭据现按平台规范落在标准配置目录）", legacy, credentialsPath)
			}
		}
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
	referenced := runtimeChannelFilter(runtime)
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

// runtimeChannelFilter 返回本次 runtime 会启动的通道键集合。runtime.Channels
// 为空表示「channels 池里全部启用的都起」，所以空引用集不能当成「一条都不起」。
func runtimeChannelFilter(runtime instances.Runtime) map[string]bool {
	referenced := map[string]bool{}
	for _, key := range runtime.Channels {
		referenced[key] = true
	}
	return referenced
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
// store 是本次启动共用的凭据库：通道密钥缺省在里面，按通道键取。
func startChannelEntry(
	command *cobra.Command,
	entry instances.Channel,
	options *dispatcherOptions,
	chat *daemon.Chat,
	store *credentials.File,
	logf func(string, ...any),
) error {
	key := entry.Key()
	// 密钥的 $VAR/${VAR} 引用在消费点展开（File 里的原始定义不动）；未定义
	// 变量在这里拦下，不让字面量发往平台。config.json 没写就按通道键回退凭据库。
	resolved, err := resolveChannelSecret(entry, store)
	if err != nil {
		return fmt.Errorf("通道 %s 启动失败：%w", key, err)
	}
	if entry.Type == instances.ChannelGitea {
		// gitea 通道不走对话桥，令牌由调度引擎按用途解析；这里只展开 $VAR
		expanded, err := envref.Expand(entry.Token, envref.Options{
			Field: fmt.Sprintf("channels[%s].token", key),
		})
		if err != nil {
			return fmt.Errorf("通道 %s 启动失败：%w", key, err)
		}
		entry.Token = expanded
	} else {
		if !resolved.complete() {
			return missingChannelSecret(entry)
		}
		entry = applyResolvedChannel(entry, resolved)
	}
	switch entry.Type {
	case instances.ChannelWeixin:
		channel := weixin.NewAdapter(weixin.AdapterConfig{
			Name: key,
			ClientConfig: weixin.Config{
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
		channel := qq.NewAdapter(qq.AdapterConfig{
			AppID:      entry.AppID,
			AppSecret:  entry.AppSecret,
			APIBaseURL: entry.APIBaseURL,
			Sandbox:    entry.Sandbox,
			AdminUsers: entry.AdminUsers,
			SplitLimit: entry.SplitLimit,
		}, key, options.Debug, logf)
		startChannel(command, channel, chat, options.Debug, logf)
	case instances.ChannelTelegram:
		channel := telegram.NewAdapter(telegram.AdapterConfig{
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
	channel integration.ChatIntegration,
	chat *daemon.Chat,
	debug bool,
	logf func(string, ...any),
) {
	config := integration.Options{Conversations: daemon.IntegrateConversations(chat), Log: logf, Debug: debug}
	go func() {
		if err := integration.RunChat(command.Context(), channel, config); err != nil {
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

// resolveSessionsStorage 解析会话归档的对象存储配置：来自 config.json 的 sessions
// 节（access_key / secret_key 经 $VAR/${VAR} 展开）。返回配置与来源标签。
//
// 归档只有这一个配置来源：不再有环境变量与 sidecar 文件（旧 serve.json /
// sessions-remote.json 已随两个子命令删除，无迁移期）。
func resolveSessionsStorage(file *instances.File) (sessionstore.StorageConfig, string) {
	if file.Sessions == nil || file.Sessions.Storage == nil {
		return sessionstore.StorageConfig{}, "未配置"
	}
	storage := file.Sessions.Storage
	config := sessionstore.StorageConfig{
		Endpoint: storage.Endpoint,
		Bucket:   storage.Bucket,
		Secure:   storage.Secure,
		Region:   storage.Region,
		Prefix:   storage.Prefix,
	}
	// 装载期 validateSecretRefs 已拦截未定义引用；这里失败就保留原值兜底
	if expanded, err := envref.Expand(storage.AccessKey, envref.Options{Field: "sessions.storage.access_key"}); err == nil {
		config.AccessKey = expanded
	} else {
		config.AccessKey = storage.AccessKey
	}
	if expanded, err := envref.Expand(storage.SecretKey, envref.Options{Field: "sessions.storage.secret_key"}); err == nil {
		config.SecretKey = expanded
	} else {
		config.SecretKey = storage.SecretKey
	}
	if !config.Enabled() {
		return sessionstore.StorageConfig{}, "未配置"
	}
	return config, "config"
}

// buildSessionArchive 组装归档器：对象存储 + 本地索引。任一步失败都只返回错误，
// 由调用方告警后以"归档未启用"继续（归档是增量能力，不该拖垮对话会话）。
func buildSessionArchive(
	ctx context.Context,
	runtime instances.Runtime,
	storage sessionstore.StorageConfig,
	sessionDir, stateDir string,
	logf func(string, ...any),
) (*sessionstore.Archiver, error) {
	if !storage.Enabled() {
		return nil, nil
	}
	blob, err := sessionstore.NewS3Store(storage)
	if err != nil {
		return nil, err
	}
	// 启动期确认桶存在：桶名写错要在这里就报出来，而不是等第一条会话归档才失败
	if err := blob.EnsureBucket(ctx); err != nil {
		return nil, err
	}
	indexPath := runtime.SessionsIndexPath()
	if indexPath == "" {
		return nil, fmt.Errorf("状态库已关闭，无法确定会话索引落点")
	}
	index, err := sessionindex.Open(indexPath)
	if err != nil {
		return nil, err
	}
	logf("会话归档：索引 %s，桶 %s（前缀 %q）", indexPath, storage.Bucket, storage.Prefix)
	return sessionstore.NewArchiver(sessionstore.CollectOptions{
		Root:    sessionDir,
		ChatDir: stateDir,
	}, blob, index, logf), nil
}

// startSessionArchiveLoop 起后台巡检：立即全量归档一次，之后每 archiveInterval
// 再跑一遍，捞每轮钩子漏掉的评审/分诊会话。错误只记日志，循环不退出（与调度
// 引擎的 ticker 同构）。归档器为 nil（未启用）时是空操作。
func startSessionArchiveLoop(ctx context.Context, archive *sessionstore.Archiver, logf func(string, ...any)) {
	if archive == nil {
		return
	}
	go func() {
		defer archive.Close()
		ticker := time.NewTicker(archiveInterval)
		defer ticker.Stop()
		for {
			summary, err := archive.Archive(ctx)
			switch {
			case err != nil:
				logf("会话归档巡检：扫描 %d，归档 %d，跳过 %d，失败 %d（%v）",
					summary.Scanned, summary.Stored, summary.Skipped, summary.Failed, err)
			case summary.Stored > 0:
				logf("会话归档巡检：归档 %d 条（扫描 %d，跳过 %d）",
					summary.Stored, summary.Scanned, summary.Skipped)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// archiveInterval 是后台归档巡检的间隔。
const archiveInterval = 5 * time.Minute

// logChatSummary 打印对话会话的前提：claude 与最小模式、主 agent 与子代理、
// 记录归档去向（来源标明 config/未配置）。一次说清，别让「为什么没生效」靠猜。
func logChatSummary(
	logf func(string, ...any),
	mainAgent daemon.AgentRuntime,
	subagents []daemon.SubagentDefinition,
	chat *daemon.Chat,
	archive *sessionstore.Archiver,
	storage sessionstore.StorageConfig,
	storageSource string,
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
	if archive != nil {
		logf("  记录归档   = s3://%s/%s%s（来源 %s；每轮结束归档该会话，另有 5 分钟巡检）",
			storage.Bucket, storage.Prefix, prefixSlash(storage.Prefix), storageSource)
	} else {
		logf("  记录归档   = 未配置（config.json 的 sessions.storage 给 endpoint 与 bucket 即启用）")
	}
}

// prefixSlash 在非空前缀后补一个斜杠，只为日志里把 <prefix> 与 <host>/... 分开。
func prefixSlash(prefix string) string {
	if strings.TrimSpace(prefix) == "" {
		return ""
	}
	return "/"
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
