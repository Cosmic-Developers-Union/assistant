package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	builtinagents "github.com/Cosmic-Developers-Union/assistant/internal/agents"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/daemon"
	"github.com/Cosmic-Developers-Union/assistant/internal/envref"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration"
	"github.com/Cosmic-Developers-Union/assistant/internal/provider"

	"github.com/spf13/cobra"
)

// endpointPathHint 是 daemon MCP 的 Long 文案里写给用户的端点落点：它必须与
// 运行期真正的端点解析同源，否则操作者照着文档去找 daemon.json 会找不到文件，
// 而 MCP 的「自动发现」就变成了猜。
func TestEndpointPathHintFollowsConfigAnchor(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ASSISTANT_CONFIG", configPath)

	want, err := instances.DaemonEndpointPath()
	if err != nil {
		t.Fatalf("DaemonEndpointPath() error = %v", err)
	}
	if want != filepath.Join(filepath.Dir(configPath), "daemon.json") {
		t.Fatalf("前置条件不成立：端点应锚定 --config/ASSISTANT_CONFIG 同目录，got %q", want)
	}
	if got := endpointPathHint(); got != want {
		t.Errorf("endpointPathHint() = %q, want %q（提示必须与运行期落点一致）", got, want)
	}
}

// daemon MCP 是会话内唯一自举的状态查询工具面：命令名、拒收多余参数、以及
// Long 文案里必须带上端点落点提示（用户看不到端点文件在哪里就无从排查
// 「daemon 未运行」），这些都是对模型/操作者的公开契约。
func TestNewDaemonMCPCommandSurface(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ASSISTANT_CONFIG", configPath)

	command := newDaemonMCPCommand()
	if command.Use != "daemon" {
		t.Errorf("Use = %q, want daemon（MCP server 名由它决定）", command.Use)
	}
	if command.RunE == nil {
		t.Fatal("RunE = nil：命令将不可执行")
	}
	hint := endpointPathHint()
	if !strings.Contains(command.Long, hint) {
		t.Errorf("Long 未嵌入端点提示 %q：\n%s", hint, command.Long)
	}
	// 工具清单是给模型看的契约，掉一个都会让模型找不到查询入口
	for _, tool := range []string{"daemon_status", "list_sessions", "list_queue", "recent_results"} {
		if !strings.Contains(command.Long, tool) {
			t.Errorf("Long 未列出工具 %q：\n%s", tool, command.Long)
		}
	}
	// 端点/地址的环境变量覆盖也必须写明，否则容器里换地址只能改配置
	for _, key := range []string{"ASSISTANT_DAEMON_ENDPOINT", "ASSISTANT_DAEMON_ADDR"} {
		if !strings.Contains(command.Long, key) {
			t.Errorf("Long 未说明覆盖项 %q：\n%s", key, command.Long)
		}
	}
	// cobra.NoArgs：daemon MCP 以 stdio 运行，任何位置参数都是配置错误
	if err := command.Args(command, []string{"extra"}); err == nil {
		t.Error("多余位置参数应被拒绝（stdio MCP 不接受参数）")
	}
}

// weixin status 在「配置里根本没有通道」与「有配置但一条 weixin 都没有」两种
// 情况都必须给出同一句可执行的下一步（先 login）——两者的排查动作相同，
// 但都不能静默退出，否则用户以为桥已在跑。
func TestWeixinStatusReportsMissingBridge(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const want = "未配置 weixin 通道：运行 assistant login add --type weixin 登记凭据"

	t.Run("指向已删除文件", func(t *testing.T) {
		// ASSISTANT_CONFIG 指着一个不存在的路径时，resolveInstanceFile 会直接
		// 把 os.ReadFile 的错误抛出来（不是「没有配置」）——这条路径上必须报错
		// 退出而不是假装没配置，否则用户会以为桥只是没配、反复重跑登录。
		missing := filepath.Join(t.TempDir(), "does-not-exist.json")
		t.Setenv("ASSISTANT_CONFIG", missing)
		if _, err := runLoginList(t, "", "--type", "weixin"); err == nil {
			t.Fatal("配置路径不可读时应报错")
		}
	})

	t.Run("有配置但没有 weixin 通道", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")
		t.Setenv("ASSISTANT_CONFIG", configPath)
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelQQ, AppID: "1", AppSecret: "s"},
		}}
		if err := instances.Save(configPath, file); err != nil {
			t.Fatal(err)
		}
		out, err := runLoginList(t, "", "--type", "weixin")
		if err != nil {
			t.Fatalf("只有 qq 通道时不应报错：%v", err)
		}
		if !strings.Contains(out, want) {
			t.Errorf("输出 = %q, want 含 %q", out, want)
		}
		// qq 通道不属于微信桥，绝不能出现在输出里
		if strings.Contains(out, "qq") {
			t.Errorf("输出串入了非 weixin 通道：%q", out)
		}
	})
}

// weixin status 的核心安全承诺是「不显示令牌」：只报 set/none 状态，令牌本身
// 一个字符都不能落进终端或日志（status 常被贴进 issue）。
func TestWeixinStatusNeverLeaksToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const secret = "wx-super-secret-token-value"
	disabled := false
	file := &instances.File{Channels: []instances.Channel{
		{
			Type: instances.ChannelWeixin, Name: "work", BaseURL: "https://ilink.example.com",
			BotToken: secret, BotID: "bot-42", LoginUserID: "user-7",
			AdminUsers: []string{"alice", "bob"},
		},
		{
			// 未启用 + 未登录 + 空 BotID：走 orDash 的占位分支。bot_token 是配置
			// 校验必填项，这里给一个与 secret 无关的值，验证输出只报状态。
			Type: instances.ChannelWeixin, Enabled: &disabled, BotToken: "other-token",
		},
	}}
	// 显式把配置路径交给命令（root 的 --config 就是这样透传下来的），既走通
	// 「读取 → 解析 → status 打印」全链路，又不碰进程共享的标准配置落点——
	// 那个位置会被同包其它测试当作「无配置」的前提。
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	got, err := runLoginList(t, configPath, "--type", "weixin")
	if err != nil {
		t.Fatalf("list --type weixin: %v", err)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("输出泄漏了令牌：%q", got)
	}
	// 命名实例：键含实例名、令牌状态 set、凭据字段原样展示（bot_id/login/admins）
	for _, want := range []string{
		"key=weixin/work", "enabled=true", "base_url=https://ilink.example.com",
		"login=user-7", "bot_id=bot-42", "token=set", "admins=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q：\n%s", want, got)
		}
	}
	// 匿名实例：未启用、缺省字段用 "-" 占位、空令牌报 none
	for _, want := range []string{
		"enabled=false", "login=-", "bot_id=-", "token=set", "admins=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("匿名实例输出缺少 %q：\n%s", want, got)
		}
	}
}

// weixin 登录的旗标面是对外契约：默认端点必须写进帮助文本（用户要知道不传
// --base-url 时打到哪），--name 决定凭据写进匿名还是命名实例（会话键随之改变）。
// 两个旗标都只对 weixin 有意义，传给别的 --type 会被显式拒绝。
func TestWeixinLoginFlagSurface(t *testing.T) {
	configPath := ""
	command := newLoginCommand(&configPath)
	add, _, err := command.Find([]string{"add"})
	if err != nil {
		t.Fatalf("Find(add): %v", err)
	}
	baseURL := add.Flags().Lookup("base-url")
	if baseURL == nil {
		t.Fatal("--base-url 未注册")
	}
	if baseURL.DefValue != "" {
		t.Errorf("--base-url 默认值 = %q, want 空（缺省端点由登录流程补齐）", baseURL.DefValue)
	}
	if !strings.Contains(baseURL.Usage, instances.DefaultWeixinBaseURL) {
		t.Errorf("--base-url 帮助未写出缺省端点 %q：%q", instances.DefaultWeixinBaseURL, baseURL.Usage)
	}
	name := add.Flags().Lookup("name")
	if name == nil {
		t.Fatal("--name 未注册（多微信账号靠它区分实例）")
	}
	if !strings.Contains(name.Usage, "匿名实例") {
		t.Errorf("--name 帮助未说明缺省写匿名实例：%q", name.Usage)
	}
}

// buildMainAgentRuntime 的用户覆盖语义：config.json 的 agents[main] 只要写了
// 字段就覆盖内置预设，没写的字段必须保留内置值——把没写的字段清空会让内置
// 提示词丢失，模型行为静默退化。
func TestBuildMainAgentRuntimeUserOverrideKeepsBuiltinDefaults(t *testing.T) {
	builtin, ok := builtinagents.Lookup(instances.DefaultMainAgent)
	if !ok {
		t.Fatal("内置主 agent 预设缺失")
	}
	file := &instances.File{
		Agents: map[string]instances.Agent{
			instances.DefaultMainAgent: {
				Description:  "用户写的描述",
				SystemPrompt: "用户写的提示词",
				Model:        "用户选的模型",
			},
		},
	}
	agent, err := buildMainAgentRuntime(file, instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildMainAgentRuntime() error = %v", err)
	}
	if agent.Model != "用户选的模型" {
		t.Errorf("Model = %q, want 用户覆盖值", agent.Model)
	}
	if agent.SystemPrompt != "用户写的提示词" {
		t.Errorf("SystemPrompt = %q, want 用户覆盖值", agent.SystemPrompt)
	}

	// 只覆盖一个字段时，其余字段必须继续用内置预设
	partial := &instances.File{
		Agents: map[string]instances.Agent{
			instances.DefaultMainAgent: {Model: "only-model"},
		},
	}
	agent, err = buildMainAgentRuntime(partial, instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildMainAgentRuntime() error = %v", err)
	}
	if agent.SystemPrompt != builtin.SystemPrompt {
		t.Errorf("未覆盖的 SystemPrompt 应保持内置：%q != %q", agent.SystemPrompt, builtin.SystemPrompt)
	}
	if agent.Model != "only-model" {
		t.Errorf("Model = %q, want only-model", agent.Model)
	}
}

// buildMainAgentRuntime 的 MCP 环境变量引用必须在校验期展开并给出
// agents[<名>].mcp 定位——未定义变量留到会话启动才炸的话，用户拿到的是
// claude 侧的模糊报错，找不到是哪条 agent 配置写错了。
func TestBuildMainAgentRuntimeMCPUndefinedVariableNamesField(t *testing.T) {
	unsetEnv(t, "ASSISTANT_MCP_UNSET_VAR")

	file := &instances.File{
		Agents: map[string]instances.Agent{
			instances.DefaultMainAgent: {
				MCP: map[string]any{
					"env": map[string]any{"TOKEN": "$ASSISTANT_MCP_UNSET_VAR"},
				},
			},
		},
	}
	_, err := buildMainAgentRuntime(file, instances.Runtime{}, &dispatcherOptions{})
	if err == nil {
		t.Fatal("error = nil, want 未定义环境变量报错")
	}
	var undefined *envref.UndefinedError
	if !errors.As(err, &undefined) {
		t.Fatalf("error = %v, want *envref.UndefinedError（识别错误链而非字符串匹配）", err)
	}
	if undefined.Name != "ASSISTANT_MCP_UNSET_VAR" {
		t.Errorf("UndefinedError.Name = %q, want ASSISTANT_MCP_UNSET_VAR", undefined.Name)
	}
	wantField := "agents[" + instances.DefaultMainAgent + "].mcp.env.TOKEN"
	if undefined.Field != wantField {
		t.Errorf("Field = %q, want %q（要指出是哪条 agent 的哪个键）", undefined.Field, wantField)
	}
}

// buildMainAgentRuntime 的 provider 链：写 provider 的 agent 用它自己的，
// 没写 provider 的 agent 由 runtime.provider 接管（而不是掉到全局默认）——
// runtime 级 provider 是用户声明「这个运行时的会话都走这家」的唯一手段。
func TestBuildMainAgentRuntimeProviderPrecedence(t *testing.T) {
	providers := map[string]instances.Provider{
		"runtime": {APIKey: "runtime-key"},
		"agent":   {APIKey: "agent-key"},
		"global":  {APIKey: "global-key"},
	}

	// agent 显式写 provider ⇒ agent 优先
	withProvider := &instances.File{
		Providers:       providers,
		DefaultProvider: "global",
		Agents: map[string]instances.Agent{
			instances.DefaultMainAgent: {Provider: "agent"},
		},
	}
	agent, err := buildMainAgentRuntime(withProvider, instances.Runtime{Provider: "runtime"}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildMainAgentRuntime() error = %v", err)
	}
	if agent.ProviderName != "agent" {
		t.Errorf("ProviderName = %q, want agent（agent 显式 provider 优先于 runtime）", agent.ProviderName)
	}

	// agent 没写 provider ⇒ runtime.provider 接管
	plain := &instances.File{Providers: providers, DefaultProvider: "global"}
	agent, err = buildMainAgentRuntime(plain, instances.Runtime{Provider: "runtime"}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildMainAgentRuntime() error = %v", err)
	}
	if agent.ProviderName != "runtime" {
		t.Errorf("ProviderName = %q, want runtime（runtime.provider 应接管未写 provider 的 agent）", agent.ProviderName)
	}

	// 都没有 ⇒ 全局默认
	agent, err = buildMainAgentRuntime(plain, instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildMainAgentRuntime() error = %v", err)
	}
	if agent.ProviderName != "global" {
		t.Errorf("ProviderName = %q, want global（回落到全局默认）", agent.ProviderName)
	}
}

// buildSubagents 的用户覆盖只作用于同名 agent，其余内置子代理的定义必须原样
// 保留（Prompt 尤其不能被清空）：漏掉或清空一项能力都属于静默缩水。
func TestBuildSubagentsUserOverrideIsScoped(t *testing.T) {
	builtinList := builtinagents.Subagents()
	if len(builtinList) == 0 {
		t.Skip("没有内置子代理可测")
	}
	target := builtinList[0]
	file := &instances.File{
		Agents: map[string]instances.Agent{
			target.Name: {Description: "用户改写的描述", Model: "用户选的模型"},
		},
	}
	subagents, err := buildSubagents(file, instances.Runtime{Subagents: []string{target.Name}}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildSubagents() error = %v", err)
	}
	if len(subagents) != 1 {
		t.Fatalf("子代理数 = %d, want 1", len(subagents))
	}
	if subagents[0].Description != "用户改写的描述" {
		t.Errorf("Description = %q, want 用户覆盖值", subagents[0].Description)
	}
	if subagents[0].Model != "用户选的模型" {
		t.Errorf("Model = %q, want 用户覆盖值", subagents[0].Model)
	}
	// 未覆盖的提示词继续用内置预设（清空提示词会让子代理行为退化）
	if subagents[0].Prompt != target.SystemPrompt {
		t.Errorf("Prompt 应保持内置预设：%q != %q", subagents[0].Prompt, target.SystemPrompt)
	}

	// 全部内置子代理都要出现在缺省清单里
	all, err := buildSubagents(&instances.File{}, instances.Runtime{Subagents: nil}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("buildSubagents() error = %v", err)
	}
	names := subagentNames(all)
	for _, want := range builtinagents.SubagentNames() {
		if !containsString(names, want) {
			t.Errorf("缺省子代理清单缺少 %q：%v", want, names)
		}
	}
}

// buildSubagents 的 MCP 引用与主 agent 同口径：报错必须点名
// agents[<子代理名>].mcp，否则多子代理时无法定位是哪一条写坏了。
func TestBuildSubagentsMCPUndefinedVariableNamesField(t *testing.T) {
	unsetEnv(t, "ASSISTANT_SUB_MCP_UNSET")

	file := &instances.File{
		Agents: map[string]instances.Agent{
			"coder": {
				MCP: map[string]any{
					"env": map[string]any{"KEY": "${ASSISTANT_SUB_MCP_UNSET}"},
				},
			},
		},
	}
	_, err := buildSubagents(file, instances.Runtime{Subagents: []string{"coder"}}, &dispatcherOptions{})
	if err == nil {
		t.Fatal("error = nil, want 未定义环境变量报错")
	}
	var undefined *envref.UndefinedError
	if !errors.As(err, &undefined) {
		t.Fatalf("error = %v, want *envref.UndefinedError", err)
	}
	if undefined.Field != "agents[coder].mcp.env.KEY" {
		t.Errorf("Field = %q, want agents[coder].mcp.env.KEY", undefined.Field)
	}
}

// agentRuntimeFromDefinition 的 claude_bin 优先级与 bare 探测：agent 级 > 命令行
// 旗标 > "claude"。bare 是「这个二进制支不支持 --bare」的探测结果，必须跟着
// 最终选中的二进制走——跟着旗标走会让 agent 级的自定义二进制拿到错误模式。
func TestAgentRuntimeFromDefinitionClaudeBinPrecedence(t *testing.T) {
	definition := builtinagents.Definition{Name: instances.DefaultMainAgent}

	t.Run("agent 级 claude_bin 优先于旗标", func(t *testing.T) {
		file := &instances.File{
			Agents: map[string]instances.Agent{
				instances.DefaultMainAgent: {ClaudeBin: "/opt/agent-claude"},
			},
		}
		agent, err := agentRuntimeFromDefinition(file, definition, instances.Runtime{},
			&dispatcherOptions{ClaudeBin: "/opt/flag-claude"})
		if err != nil {
			t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
		}
		if agent.ClaudeBin != "/opt/agent-claude" {
			t.Errorf("ClaudeBin = %q, want agent 级", agent.ClaudeBin)
		}
		if agent.Bare != claudecfg.SupportsBare("/opt/agent-claude") {
			t.Errorf("Bare = %v, want 跟着选中的二进制探测", agent.Bare)
		}
	})

	t.Run("旗标兜底", func(t *testing.T) {
		agent, err := agentRuntimeFromDefinition(&instances.File{}, definition, instances.Runtime{},
			&dispatcherOptions{ClaudeBin: "/opt/flag-claude"})
		if err != nil {
			t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
		}
		if agent.ClaudeBin != "/opt/flag-claude" {
			t.Errorf("ClaudeBin = %q, want 旗标值", agent.ClaudeBin)
		}
	})

	t.Run("都空时回落 claude", func(t *testing.T) {
		agent, err := agentRuntimeFromDefinition(&instances.File{}, definition, instances.Runtime{},
			&dispatcherOptions{})
		if err != nil {
			t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
		}
		if agent.ClaudeBin != "claude" {
			t.Errorf("ClaudeBin = %q, want claude", agent.ClaudeBin)
		}
	})
}

// agent 级 session_timeout_ms 必须落成 Timeout，否则用户在 config.json 里调的
// 超时会被忽略、会话按内置缺省被砍断；未配置时留 0 由 ChatConfig 兜底。
func TestAgentRuntimeFromDefinitionSessionTimeout(t *testing.T) {
	file := &instances.File{
		Agents: map[string]instances.Agent{
			instances.DefaultMainAgent: {SessionTimeoutMS: 45000},
		},
	}
	agent, err := agentRuntimeFromDefinition(file, builtinagents.Definition{Name: instances.DefaultMainAgent},
		instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
	}
	if agent.Timeout != 45*time.Second {
		t.Errorf("Timeout = %v, want 45s（毫秒配置要按毫秒换算）", agent.Timeout)
	}

	// 未配置 ⇒ 0（交给 ChatConfig 的缺省，而不是在这里替用户决定）
	zero, err := agentRuntimeFromDefinition(&instances.File{},
		builtinagents.Definition{Name: instances.DefaultMainAgent}, instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
	}
	if zero.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0（未配置时不该擅自给值）", zero.Timeout)
	}
}

// provider 解析失败（optimizations 里的密钥引用未定义）必须包上
// agents[<名>] provider <名> 的语境：直接抛底层错误会让用户不知道是哪个 agent
// 的 provider 链出的问题。
func TestAgentRuntimeFromDefinitionWrapsProviderError(t *testing.T) {
	unsetEnv(t, "ASSISTANT_PROVIDER_UNSET")

	file := &instances.File{
		Optimizations: instances.Provider{Env: map[string]string{"ANTHROPIC_API_KEY": "$ASSISTANT_PROVIDER_UNSET"}},
	}
	_, err := agentRuntimeFromDefinition(file, builtinagents.Definition{Name: instances.DefaultMainAgent},
		instances.Runtime{}, &dispatcherOptions{})
	if err == nil {
		t.Fatal("error = nil, want provider 解析失败")
	}
	if !strings.Contains(err.Error(), "agents[") || !strings.Contains(err.Error(), "provider") {
		t.Errorf("error = %v, want 带 agents[<名>] provider <名> 语境", err)
	}
}

// agentRuntimeFromDefinition 合并 definition.MCP 时会原地写入 overrides.MCP：
// 定义里的 MCP server 必须出现在生效覆盖里（子代理/主 agent 的工具面靠它），
// 且 overrides.MCP 为 nil 时要能建出 map 而不是 panic。
func TestAgentRuntimeFromDefinitionMergesMCP(t *testing.T) {
	file := &instances.File{
		Providers: map[string]instances.Provider{"p": {}},
		Agents:    map[string]instances.Agent{instances.DefaultMainAgent: {Provider: "p"}},
	}
	definition := builtinagents.Definition{
		Name: instances.DefaultMainAgent,
		MCP:  map[string]any{"gitea": map[string]any{"command": "assistant"}},
	}
	agent, err := agentRuntimeFromDefinition(file, definition, instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
	}
	if _, ok := agent.Provider.MCP["gitea"]; !ok {
		t.Errorf("MCP 未合并进生效覆盖：%#v", agent.Provider.MCP)
	}

	// 空 MCP 定义不改变 overrides（不应凭空造出 map 掩盖「没配」）
	agent, err = agentRuntimeFromDefinition(file, builtinagents.Definition{Name: instances.DefaultMainAgent},
		instances.Runtime{}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("agentRuntimeFromDefinition() error = %v", err)
	}
	if len(agent.Provider.MCP) != 0 {
		t.Errorf("空 MCP 定义不应改动静默状态：%#v", agent.Provider.MCP)
	}
}

// checkChatCredentials 的「没有凭据」这一路：daemon 必须用警告把
// 「对话会认证失败」说在启动时（而不是让每条消息静默重试几分钟），警告走
// stderr、且不算错误（缺凭据不拦启动，用户可能只是先跑空壳）。
func TestCheckChatCredentialsWarnsWithoutCredentials(t *testing.T) {
	isolateCredentials(t)
	for _, key := range claudecfg.CredentialEnvKeys {
		t.Setenv(key, "")
	}

	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	var logs []string

	checkChatCredentials(command, &instances.File{}, daemon.AgentRuntime{Name: "main"},
		nil, func(format string, arguments ...any) { logs = append(logs, format) })

	got := stderr.String()
	if !strings.Contains(got, "警告：") {
		t.Errorf("stderr = %q, want 带「警告：」前缀", got)
	}
	if !strings.Contains(got, "没有可用的 AI 凭据") {
		t.Errorf("stderr = %q, want 说清对话会认证失败", got)
	}
	if !strings.Contains(got, claudecfg.MissingCredentialHint) {
		t.Errorf("stderr = %q, want 附上可执行的补救提示", got)
	}
	if len(logs) != 0 {
		t.Errorf("缺凭据时不应走成功日志：%v", logs)
	}
}

// checkChatCredentials 的自检失败路径：有凭据但端点不配套（这里打到不可达的
// 本地端口）时必须告警并附 provider.CredentialHint——这正是「每条消息静默
// 重试几分钟」那个故障的启动期拦截点。
func TestCheckChatCredentialsWarnsOnFailedSelfCheck(t *testing.T) {
	isolateCredentials(t)
	for _, key := range claudecfg.CredentialEnvKeys {
		t.Setenv(key, "")
	}

	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	var logs []string

	overrides := claudecfg.Overrides{Env: map[string]string{
		"ANTHROPIC_API_KEY":  "sk-test",
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:1",
	}}
	checkChatCredentials(command, &instances.File{},
		daemon.AgentRuntime{Name: "main", Provider: overrides, ProviderName: "anthropic"},
		nil, func(format string, arguments ...any) { logs = append(logs, format) })

	got := stderr.String()
	if !strings.Contains(got, "凭据自检失败") {
		t.Errorf("stderr = %q, want 报告自检失败", got)
	}
	if !strings.Contains(got, provider.CredentialHint) {
		t.Errorf("stderr = %q, want 附上 provider.CredentialHint", got)
	}
	if len(logs) != 0 {
		t.Errorf("自检失败时不应走成功日志：%v", logs)
	}
}

// startChannel 的契约：它是「通用桥 + 后台协程」的包装，绝不能在调用线程上
// 阻塞（否则第一条通道就把 daemon 卡死在启动里）。用非 nil 的 Channel 配
// 无效的 Chat 触发协程内的即刻退出，验证调用立即返回且退出原因写进日志。
func TestStartChannelReturnsImmediatelyAndLogsExit(t *testing.T) {
	command := &cobra.Command{}
	command.SetContext(t.Context())

	logs := make(chan string, 8)
	logf := func(format string, arguments ...any) {
		select {
		case logs <- format:
		default:
		}
	}
	done := make(chan struct{})
	go func() {
		// Chat 为 nil ⇒ RunChannel 立刻返回「对话会话未初始化」，协程立即结束
		startChannel(command, stubChannel{name: "test"}, nil, false, logf)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startChannel 在调用线程上阻塞了：daemon 启动会被第一条通道卡死")
	}

	select {
	case got := <-logs:
		// 只要求日志指明通道且带上原因：退出路径的具体措辞由 integration 决定
		// （接收失败会退避重试，只有致命错误才「退出」），这里钉的是「不会静默」。
		if !strings.Contains(got, "通道 %s") || !strings.Contains(got, "%v") {
			t.Errorf("日志格式 = %q，want 指明通道与原因", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("通道退出未写日志：操作者会以为通道还在跑")
	}
}

// startChannelEntry 是 channels 列表的启动分派，两个契约必须钉死：
//  1. telegram/weixin 这类对话桥一律走 startChannel（后台协程），所以 chat 为
//     nil 时它仍应立即返回 nil——「已启动」是分派成功的返回值，而不是等通道跑完；
//  2. gitea 通道绝不能经过对话桥启动（它归调度引擎），必须显式报错而不是静默跳过，
//     否则配错位置会让评审调度凭空消失；未知类型同理，必须点名平台类型。
func TestStartChannelEntryDispatchContract(t *testing.T) {
	command := &cobra.Command{}
	command.SetContext(t.Context())
	options := &dispatcherOptions{}
	logf := func(string, ...any) {}

	t.Run("telegram 桥走后台协程并返回已启动", func(t *testing.T) {
		entry := instances.Channel{
			Type: instances.ChannelTelegram, Name: "alerts",
			BotToken: "123:abc", AdminUsers: []string{"alice"},
		}
		if err := startChannelEntry(command, entry, options, nil, &credentials.File{}, logf); err != nil {
			t.Fatalf("对话桥分派不应报错：%v", err)
		}
	})

	t.Run("gitea 通道不得走对话桥", func(t *testing.T) {
		entry := instances.Channel{Type: instances.ChannelGitea, Host: "https://git.example.com"}
		err := startChannelEntry(command, entry, options, nil, &credentials.File{}, logf)
		if err == nil {
			t.Fatal("gitea 通道应由调度引擎接管，startChannelEntry 必须报错")
		}
		if !strings.Contains(err.Error(), "调度引擎") {
			t.Errorf("报错 = %q，want 点明由调度引擎接管", err)
		}
	})

	t.Run("未知平台类型点名类型", func(t *testing.T) {
		entry := instances.Channel{Type: "slack"}
		err := startChannelEntry(command, entry, options, nil, &credentials.File{}, logf)
		if err == nil {
			t.Fatal("未知平台类型必须报错，不能静默忽略")
		}
		if !strings.Contains(err.Error(), "slack") {
			t.Errorf("报错 = %q，want 含类型名（用户据此定位配置笔误）", err)
		}
	})
}

// stubChannel 是最小 integration.ChatIntegration：只提供 Name（startChannel 的退出日志要用）。
type stubChannel struct{ name string }

func (c stubChannel) Name() string { return c.name }

func (c stubChannel) Allowed(string) (bool, string) { return true, "" }

func (c stubChannel) Receive(context.Context) (integration.Inbound, error) {
	return nil, os.ErrDeadlineExceeded
}

func (c stubChannel) SplitLimit() int { return 0 }

// containsString 是简单的包含判断（避免为一行断言引入切片工具）。
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// unsetEnv 真正移除环境变量（t.Setenv 只能把它设成空串，而空串在 envref 眼里
// 仍是「已定义」，会让未定义变量的报错路径测不到）。测试结束时按原值原样恢复，
// 避免污染同进程的其它测试。
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("移除环境变量 %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			if err := os.Setenv(key, previous); err != nil {
				t.Errorf("恢复环境变量 %s: %v", key, err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Errorf("清理环境变量 %s: %v", key, err)
		}
	})
}
