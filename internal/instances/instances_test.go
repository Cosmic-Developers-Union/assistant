package instances

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"assistant/internal/envref"
)

// 载入即规范化：instances 迁移为 gitea 通道（host 去尾斜杠、账号缺省、repo
// 简写展开），并合成缺省 runtime。
func TestLoadNormalizesDefaultsAndRepoShorthand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "instances": [
    {
      "host": "https://gitea.example.com/",
      "repos": ["owner/repo", {"name": "owner/another", "dir": "/srv/another"}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(file.Instances) != 0 {
		t.Errorf("instances 应已迁移清空：%+v", file.Instances)
	}
	if len(file.Channels) != 1 {
		t.Fatalf("Channels = %+v", file.Channels)
	}
	channel := file.Channels[0]
	if channel.Type != ChannelGitea || channel.Host != "https://gitea.example.com" {
		t.Errorf("channel = %+v", channel)
	}
	if channel.Reviewer != DefaultReviewerName || channel.Merger != DefaultMergerName {
		t.Errorf("account defaults = %q/%q", channel.Reviewer, channel.Merger)
	}
	if len(channel.Repos) != 2 {
		t.Fatalf("Repos = %+v", channel.Repos)
	}
	if channel.Repos[0].Name != "owner/repo" || channel.Repos[0].Dir != "" {
		t.Errorf("Repos[0] = %+v", channel.Repos[0])
	}
	if channel.Repos[1].Name != "owner/another" || channel.Repos[1].Dir != "/srv/another" {
		t.Errorf("Repos[1] = %+v", channel.Repos[1])
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if len(runtime.Channels) != 1 || runtime.Channels[0] != "gitea" {
		t.Errorf("合成 runtime 应引用全部通道：%+v", runtime.Channels)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := map[string]string{
		"empty instances": `{"instances": []}`,
		"bad host":        `{"instances": [{"host": "gitea.example.com", "repos": []}]}`,
		"duplicate host": `{"instances": [
			{"host": "https://a.example.com", "repos": []},
			{"host": "https://a.example.com", "repos": []}
		]}`,
		"bad repository": `{"instances": [{"host": "https://a.example.com", "repos": ["nope"]}]}`,
		"same account": `{"instances": [{"host": "https://a.example.com", "repos": [],
			"reviewer": {"name": "bot"}, "merger": {"name": "bot"}}]}`,
		"unknown field": `{"instances": [{"host": "https://a.example.com", "repos": [], "token": "x"}]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Errorf("Load(%s) error = nil, want error", content)
			}
		})
	}
}

func TestSaveWritesRestrictedFileAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	file := &File{Instances: []Instance{{
		Host:     "https://gitea.example.com",
		Reviewer: Account{Name: "ai"},
		Merger:   Account{Name: "merge"},
		Repos:    []Repo{{Name: "owner/repo"}, {Name: "owner/another", Dir: "/srv/another"}},
	}}}
	file.Normalize()
	if err := file.canonicalize(t.TempDir()); err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if err := Save(path, file); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Errorf("permissions = %o, want 600", permission)
	}
	// 规范形写回：instances 消失，gitea 通道落地
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"instances"`) {
		t.Errorf("规范形不应再写 instances：\n%s", raw)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Channels) != 1 || loaded.Channels[0].Type != ChannelGitea {
		t.Fatalf("Channels = %+v", loaded.Channels)
	}
	channel := loaded.Channels[0]
	if channel.Reviewer != "ai" || channel.Merger != "merge" {
		t.Errorf("account round-trip failed: %q/%q", channel.Reviewer, channel.Merger)
	}
	if len(channel.Repos) != 2 ||
		channel.Repos[0].Name != "owner/repo" ||
		channel.Repos[1].Dir != "/srv/another" {
		t.Errorf("repo round-trip failed: %+v", channel.Repos)
	}
	// 凭据已迁出配置：配置里不应再出现任何令牌字段。
	if strings.Contains(string(raw), "token") {
		t.Errorf("凭据不应再写入 config.json：\n%s", raw)
	}
}

func TestRepoJSONShapes(t *testing.T) {
	var shorthand Repo
	if err := json.Unmarshal([]byte(`"owner/repo"`), &shorthand); err != nil {
		t.Fatal(err)
	}
	if shorthand.Name != "owner/repo" || shorthand.Dir != "" {
		t.Errorf("shorthand = %+v", shorthand)
	}
	encoded, err := json.Marshal(shorthand)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `"owner/repo"` {
		t.Errorf("encoded = %s", encoded)
	}
	var object Repo
	if err := json.Unmarshal([]byte(`{"name":"owner/repo","dir":"/srv/repo","provider":"anthropic"}`), &object); err != nil {
		t.Fatal(err)
	}
	if object.Dir != "/srv/repo" || object.Provider != "anthropic" {
		t.Errorf("object = %+v", object)
	}
	// 有 dir/provider 时序列化为对象（不丢字段）
	encoded, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"dir":"/srv/repo"`) ||
		!strings.Contains(string(encoded), `"provider":"anthropic"`) {
		t.Errorf("encoded = %s", encoded)
	}
}

// 会话配置根兜底 = 当前目录的 claude/（显式模式；runtime.claude_dir 优先，
// $CLAUDE_CONFIG_DIR 再优先），不碰用户的 ~/.claude。
func TestClaudeDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	work := t.TempDir()
	t.Chdir(work)
	directory, err := ClaudeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(work, "claude"); directory != want {
		t.Errorf("ClaudeDir() = %q, want %q", directory, want)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/custom-claude")
	if overridden, err := ClaudeDir(); err != nil || overridden != "/tmp/custom-claude" {
		t.Errorf("ClaudeDir() 应尊重 CLAUDE_CONFIG_DIR：%q, %v", overridden, err)
	}
}

// provider 定义松弛解析：env 接受标量、settings/mcp 原样保留、未知键不丢。
func TestProviderConfigTolerantParseAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "default_provider": "gateway",
  "providers": {
    "gateway": {
      "env": {"ANTHROPIC_BASE_URL": "https://gw.example.com", "API_TIMEOUT_MS": 600000, "DEBUG": false},
      "settings": {"model": "glm-4.6", "apiKeyHelper": "/bin/echo key"},
      "mcp": {"search": {"command": "npx", "args": ["-y", "@example/search-mcp"]}},
      "note": "手写扩展键原样保留"
    }
  },
  "instances": [
    {"host": "https://gitea.example.com", "provider": "gateway", "repos": [
      {"name": "owner/repo", "provider": "gateway"},
      "owner/other"
    ]}
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	provider := file.Providers["gateway"]
	if provider.Env["ANTHROPIC_BASE_URL"] != "https://gw.example.com" ||
		provider.Env["API_TIMEOUT_MS"] != "600000" || provider.Env["DEBUG"] != "false" {
		t.Errorf("Env = %+v", provider.Env)
	}
	if provider.Settings["model"] != "glm-4.6" || provider.Settings["apiKeyHelper"] != "/bin/echo key" {
		t.Errorf("Settings = %+v", provider.Settings)
	}
	server, _ := provider.MCP["search"].(map[string]any)
	if server["command"] != "npx" {
		t.Errorf("MCP = %+v", provider.MCP)
	}
	// 选择粒度：repo > 通道 > 全局默认（instances 已迁移为 gitea 通道）
	if len(file.Channels) != 1 || len(file.Channels[0].Repos) != 2 {
		t.Fatalf("Channels = %+v", file.Channels)
	}
	instance := Instance{Host: file.Channels[0].Host, Provider: file.Channels[0].Provider}
	repo := file.Channels[0].Repos[0]
	if got := file.ProviderName(&instance, &repo); got != "gateway" {
		t.Errorf("repo provider = %q, want gateway", got)
	}
	if got := file.ProviderName(&instance, nil); got != "gateway" {
		t.Errorf("instance provider = %q, want gateway", got)
	}
	if got := file.ProviderName(&Instance{}, nil); got != "gateway" {
		t.Errorf("default provider = %q, want gateway", got)
	}
	// 未识别键写回不丢
	if err := Save(path, file); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "手写扩展键原样保留") {
		t.Errorf("未知键应原样保留：\n%s", raw)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Providers["gateway"].Settings["model"] != "glm-4.6" {
		t.Errorf("settings round-trip failed: %+v", reloaded.Providers["gateway"].Settings)
	}
}

// provider 引用必须存在；非法 env 值（嵌套对象）报错。
func TestProviderValidation(t *testing.T) {
	tests := map[string]string{
		"unknown default": `{"default_provider": "nope", "providers": {"gw": {"env": {"A": "b"}}},
			"instances": [{"host": "https://a.example.com", "repos": []}]}`,
		"unknown instance provider": `{"providers": {"gw": {}},
			"instances": [{"host": "https://a.example.com", "provider": "nope", "repos": []}]}`,
		"unknown repo provider": `{"providers": {"gw": {}},
			"instances": [{"host": "https://a.example.com", "repos": [{"name": "o/r", "provider": "nope"}]}]}`,
		"unknown weixin provider": `{"providers": {"gw": {}},
			"weixin": {"provider": "nope", "bot_token": "t"},
			"instances": [{"host": "https://a.example.com", "repos": []}]}`,
		"nested env value": `{"providers": {"gw": {"env": {"A": {"nested": true}}}},
			"instances": [{"host": "https://a.example.com", "repos": []}]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Errorf("Load(%s) error = nil, want error", content)
			}
		})
	}
	if overrides := (&File{Providers: map[string]Provider{"gw": {}}}).ProviderNameOf("gw").Overrides(); !overrides.Empty() {
		t.Errorf("空 provider 应为空覆盖：%+v", overrides)
	}
}

// 仓库自带的示例配置必须始终可加载（providers 等字段的回归护栏），并且示范
// $schema（编辑器补全的来源）。
func TestConfigExampleLoads(t *testing.T) {
	// 示例配置的密钥用 $VAR 引用示范「密钥不落配置文件」；严格校验要求变量已定义
	for _, name := range []string{
		"GITEA_REVIEW_TOKEN", "ZHIPU_API_KEY", "OPENCODE_API_KEY",
		"ANTHROPIC_API_KEY", "QQ_TOKEN", "TELEGRAM_BOT_TOKEN", "SESSIONS_TOKEN",
	} {
		t.Setenv(name, "example-"+strings.ToLower(strings.ReplaceAll(name, "_", "-")))
	}
	file, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("config.example.json 无法加载：%v", err)
	}
	if file.Schema == "" {
		t.Error("示例配置应当带上 $schema")
	}
}

// 全局优化点（optimizations）与 provider 的叠加：provider 覆盖全局同名取值，
// 未重叠部分保留；两者都为空时返回零值。
func TestEffectiveOverridesComposition(t *testing.T) {
	file := &File{
		DefaultProvider: "gateway",
		Optimizations: Provider{
			Env:      map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "high", "DISABLE_TELEMETRY": "1"},
			Settings: map[string]any{"model": "global-model"},
			MCP:      map[string]any{"search": map[string]any{"command": "global-search"}},
		},
		Providers: map[string]Provider{
			"gateway": {
				Env:      map[string]string{"ANTHROPIC_AUTH_TOKEN": "token", "CLAUDE_CODE_EFFORT_LEVEL": "max"},
				Settings: map[string]any{"apiKeyHelper": "/bin/echo key"},
			},
		},
		Instances: []Instance{{Host: "https://gitea.example.com", Repos: []Repo{{Name: "owner/repo"}}}},
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	overrides, err := file.EffectiveOverrides("gateway")
	if err != nil {
		t.Fatalf("EffectiveOverrides: %v", err)
	}
	if overrides.Env["DISABLE_TELEMETRY"] != "1" || overrides.Env["ANTHROPIC_AUTH_TOKEN"] != "token" {
		t.Errorf("Env = %+v", overrides.Env)
	}
	if overrides.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "max" {
		t.Errorf("provider 应覆盖全局同名 env：%+v", overrides.Env)
	}
	if overrides.Settings["model"] != "global-model" || overrides.Settings["apiKeyHelper"] != "/bin/echo key" {
		t.Errorf("Settings = %+v", overrides.Settings)
	}
	if _, ok := overrides.MCP["search"]; !ok {
		t.Errorf("全局 mcp server 应保留：%+v", overrides.MCP)
	}
	empty, err := (&File{}).EffectiveOverrides("")
	if err != nil {
		t.Fatalf("EffectiveOverrides: %v", err)
	}
	if !empty.Empty() {
		t.Errorf("空配置应为空覆盖：%+v", empty)
	}
	// 全局优化点非法 env 值同样在校验期报错
	bad := &File{Optimizations: Provider{Env: map[string]string{"A B": "x"}}, Instances: []Instance{{Host: "https://a.example.com"}}}
	bad.Normalize()
	if err := bad.Validate(); err == nil {
		t.Error("非法 env 键名应报错")
	}
}

// provider 只有一个落点：config.json 的 providers。遗留的 <配置目录>/providers/
// 目录不再被读取，也不影响加载（迁移提示由 assistant validate 给出）。
func TestProviderFileDirectoryIsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	config := `{"providers": {"opencode": {"api_key": "sk-zen"}},
		"instances": [{"host": "https://gitea.example.com", "repos": [{"name": "owner/repo"}]}]}`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "providers", "opencode.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(configPath)
	if err != nil {
		t.Fatalf("遗留 providers/ 目录不该影响加载：%v", err)
	}
	provider, ok := file.LookupProvider("opencode")
	if !ok || provider.Token() != "sk-zen" {
		t.Fatalf("config.json 内联 provider 未生效：%+v ok=%v", provider, ok)
	}
	// providers/ 里的名字不会被当成已定义（否则会静默用错来源）
	if err := os.WriteFile(configPath, []byte(`{"default_provider": "only-in-file",
		"instances": [{"host": "https://gitea.example.com", "repos": []}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "未在 providers 中定义") {
		t.Fatalf("文件里的 provider 不该被认出，got %v", err)
	}
}

// 预设展开在配置校验期生效：openai 缺 base_url 即便只写 api_key 也会在 Load 报错。
func TestProviderPresetValidation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	content := `{"default_provider": "openai",
		"providers": {"openai": {"api_key": "sk-x"}},
		"instances": [{"host": "https://gitea.example.com", "repos": []}]}`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
		t.Fatalf("openai 缺 base_url 应报错，got %v", err)
	}
	// 补上 base_url（翻译代理）后通过，且 api_key 简写按预设落地
	content = `{"default_provider": "openai",
		"providers": {"openai": {"api_key": "sk-x", "env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000"}}},
		"instances": [{"host": "https://gitea.example.com", "repos": []}]}`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	overrides, err := file.EffectiveOverrides("openai")
	if err != nil {
		t.Fatalf("EffectiveOverrides: %v", err)
	}
	if overrides.Env["ANTHROPIC_AUTH_TOKEN"] != "sk-x" || overrides.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4000" {
		t.Errorf("预设+简写未生效：%+v", overrides.Env)
	}
}

// agent 池的加载、默认值与校验：引用不存在的 agent/provider 直接报错；
// 遗留顶层 qq:/weixin: 节报迁移错误。
func TestLoadAgentsAndLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"providers": {"zhipu": {"api_key": "k"}},
		"agents": {
			"ops": {"provider": "zhipu", "model": "glm-4.7", "system_prompt": "你是运维。", "session_timeout_ms": 60000},
			"coder": {"model": "m-coder"}
		},
		"default_agent": "ops",
		"channels": [{"type": "qq", "app_id": "111", "app_secret": "sec"}]
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := file.AgentNames(); len(got) != 2 || got[0] != "coder" || got[1] != "ops" {
		t.Errorf("AgentNames = %v", got)
	}
	if file.Agents["ops"].Model != "glm-4.7" || file.Agents["ops"].SystemPrompt != "你是运维。" {
		t.Errorf("agents[ops] = %+v", file.Agents["ops"])
	}
	if got := file.AgentProviderName("ops"); got != "zhipu" {
		t.Errorf("AgentProviderName(ops) = %q", got)
	}
	if got := file.AgentProviderName("coder"); got != "" {
		t.Errorf("AgentProviderName(coder) 应回退全局默认 = %q", got)
	}

	// 遗留顶层节点：给出可读的迁移报错（channels 对照写法）
	for name, bad := range map[string]string{
		"legacy qq":     `{"qq": {"enabled": true, "app_id": "1", "app_secret": "s"}}`,
		"legacy weixin": `{"weixin": {"enabled": true, "bot_token": "t"}}`,
	} {
		t.Run("migration error: "+name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(badPath)
			if err == nil {
				t.Fatalf("Load 应拒绝遗留节点：%s", bad)
			}
			if !strings.Contains(err.Error(), "channels") {
				t.Errorf("迁移错误应给出 channels 对照写法：%v", err)
			}
		})
	}

	rejects := map[string]string{
		"unknown default_agent":  `{"agents": {"ops": {}}, "default_agent": "nope", "instances": []}`,
		"unknown agent provider": `{"agents": {"ops": {"provider": "nope"}}, "instances": []}`,
		"qq channel no secret":   `{"channels": [{"type": "qq", "app_id": "1"}]}`,
		"qq channel no appid":    `{"channels": [{"type": "qq", "app_secret": "s"}]}`,
		"qq negative split":      `{"channels": [{"type": "qq", "app_id": "1", "app_secret": "s", "split_limit": -5}]}`,
	}
	for name, bad := range rejects {
		t.Run(name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(badPath); err == nil {
				t.Errorf("Load 应拒绝：%s", bad)
			}
		})
	}
}

// channels 多实例通道：type/name、平台缺省值、会话键、唯一性与旧块冲突校验、
// 内置 agent 引用。
func TestLoadChannels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"channels": [
			{"type": "weixin", "name": "work", "bot_token": "wx-tok", "agent": "ops"},
			{"type": "weixin", "name": "life", "bot_token": "wx-tok-2", "admin_users": ["*"]},
			{"type": "qq", "app_id": "111", "app_secret": "sec", "agent": "coder"},
			{"type": "telegram", "bot_token": "123:ABC", "api_base_url": "https://tg.example.com"}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	channels := file.Channels
	if channels[0].Key() != "weixin/work" || channels[1].Key() != "weixin/life" {
		t.Errorf("命名实例的会话键应为 type/name：%q %q", channels[0].Key(), channels[1].Key())
	}
	if channels[2].Key() != "qq" || channels[3].Key() != "telegram" {
		t.Errorf("未命名实例的会话键应为 type：%q %q", channels[2].Key(), channels[3].Key())
	}
	if channels[0].BaseURL != DefaultWeixinBaseURL {
		t.Errorf("weixin 实例缺省 base_url 未填：%q", channels[0].BaseURL)
	}
	if channels[2].APIBaseURL != DefaultQQAPIBaseURL {
		t.Errorf("qq 实例缺省 api_base_url 未填：%q", channels[2].APIBaseURL)
	}
	if channels[3].APIBaseURL != "https://tg.example.com" {
		t.Errorf("telegram 自定义 api_base_url 丢失：%q", channels[3].APIBaseURL)
	}
	// 内置 agent（ops/coder）可直接引用；未知名仍报错
	if err := file.Validate(); err != nil {
		t.Errorf("引用内置 agent 应通过校验：%v", err)
	}

	rejects := map[string]string{
		"bad type":                `{"channels": [{"type": "slack", "bot_token": "x"}]}`,
		"weixin no token":         `{"channels": [{"type": "weixin"}]}`,
		"qq no secret":            `{"channels": [{"type": "qq", "app_id": "1"}]}`,
		"tg no token":             `{"channels": [{"type": "telegram"}]}`,
		"dup key":                 `{"channels": [{"type": "weixin", "bot_token": "a"}, {"type": "weixin", "bot_token": "b"}]}`,
		"name with slash":         `{"channels": [{"type": "weixin", "name": "a/b", "bot_token": "t"}]}`,
		"unknown agent ref":       `{"channels": [{"type": "weixin", "bot_token": "t", "agent": "nope"}]}`,
	}
	for name, bad := range rejects {
		t.Run(name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(badPath); err == nil {
				t.Errorf("Load 应拒绝：%s", bad)
			}
		})
	}
}

// 用户 agent 的 mcp 字段：原样读回（透传给运行时合并）。
func TestAgentMCPField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"agents": {"ops": {"mcp": {"search": {"command": "search-mcp", "args": ["-v"]}}}}, "channels": [{"type": "qq", "app_id": "1", "app_secret": "s"}]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mcp := file.Agents["ops"].MCP
	if mcp == nil {
		t.Fatal("agent.mcp 未读回")
	}
	search, ok := mcp["search"].(map[string]any)
	if !ok || search["command"] != "search-mcp" {
		t.Errorf("agent.mcp = %+v", mcp)
	}
}

// gitea 通道：host 校验、repo 名校验、token 的引用保留（展开在消费点）与
// enabled 开关。
func TestGiteaChannel(t *testing.T) {
	t.Setenv("ASSISTANT_TEST_TOKEN", "sec-ret")
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"channels": [
			{"type": "gitea", "host": "https://gitea.example.com/", "token": "${ASSISTANT_TEST_TOKEN}",
			 "repos": ["acme/repo"], "agent": "review"},
			{"type": "gitea", "name": "mirror", "host": "https://mirror.example.com", "repos": ["acme/b"], "enabled": false}
		],
		"runtimes": {"main": {"channels": ["gitea"]}}
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 非破坏性：File 保留原始引用，Save 不会把明文洗回配置文件；展开在
	// 消费点（cmd/assistant 的通道/调度装配）。
	if got := file.Channels[0].Token; got != "${ASSISTANT_TEST_TOKEN}" {
		t.Errorf("token 应保留原始引用（展开在消费点）：%q", got)
	}
	expanded, err := envref.Expand(file.Channels[0].Token, envref.Options{Field: "channels[gitea].token"})
	if err != nil || expanded != "sec-ret" {
		t.Errorf("消费点展开失败：%q %v", expanded, err)
	}
	if file.Channels[0].Key() != "gitea" || file.Channels[1].Key() != "gitea/mirror" {
		t.Errorf("Key = %q %q", file.Channels[0].Key(), file.Channels[1].Key())
	}
	if file.Channels[1].IsEnabled() {
		t.Error("enabled=false 的通道应报告停用")
	}
	if !file.Channels[0].IsEnabled() {
		t.Error("缺省应启用")
	}

	rejects := map[string]string{
		"no host":          `{"channels": [{"type": "gitea", "repos": ["a/b"]}]}`,
		"bad host":         `{"channels": [{"type": "gitea", "host": "gitea.example.com", "repos": ["a/b"]}]}`,
		"bad repo":         `{"channels": [{"type": "gitea", "host": "https://gitea.example.com", "repos": ["nope"]}]}`,
		"unclosed token":   `{"channels": [{"type": "gitea", "host": "https://gitea.example.com", "token": "${VAR", "repos": ["a/b"]}]}`,
		"unknown provider": `{"channels": [{"type": "gitea", "host": "https://gitea.example.com", "provider": "nope", "repos": ["a/b"]}]}`,
	}
	for name, bad := range rejects {
		t.Run(name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(badPath); err == nil {
				t.Errorf("Load 应拒绝：%s", bad)
			}
		})
	}
}

// runtime 路径解析：缺省值收敛到 $root、${VAR:-default} 展开与 $root 自引用、
// state/api_listen 的 "off" 开关。
func TestRuntimeResolution(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ASSISTANT_TEST_ROOT", "")
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"channels": [{"type": "gitea", "host": "https://gitea.example.com"}],
		"runtimes": {
			"main": {
				"root": "${ASSISTANT_TEST_ROOT:-$XDG_DATA_HOME}/assistant",
				"repos_dir": "$root/clones",
				"state_file": "off",
				"api_listen": "off",
				"interval_ms": 5000,
				"concurrency": 3
			}
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	dataRoot := filepath.Join(os.Getenv("XDG_DATA_HOME"), "assistant")
	if runtime.DataRoot() != dataRoot {
		t.Errorf("DataRoot = %q, want %q", runtime.DataRoot(), dataRoot)
	}
	if runtime.ReposRoot() != filepath.Join(dataRoot, "clones") {
		t.Errorf("$root 自引用未生效：%q", runtime.ReposRoot())
	}
	if runtime.ReviewRootDir() != filepath.Join(dataRoot, "review") {
		t.Errorf("ReviewRootDir 缺省 = %q", runtime.ReviewRootDir())
	}
	if runtime.ChatStateDir() != filepath.Join(dataRoot, "chat") {
		t.Errorf("ChatStateDir 缺省 = %q", runtime.ChatStateDir())
	}
	if runtime.StatePath() != "" || runtime.ListenAddr() != "" {
		t.Errorf("\"off\" 未关闭：%q %q", runtime.StatePath(), runtime.ListenAddr())
	}
	if runtime.Interval() != 5000 || runtime.Workers() != 3 || runtime.Timeout() != DefaultSessionTimeoutMS {
		t.Errorf("参数缺省/覆盖 = %d/%d/%d", runtime.Interval(), runtime.Workers(), runtime.Timeout())
	}
	reviewName, err := runtime.ReviewName("https://gitea.example.com", "acme/repo", "pr", 7)
	if err != nil {
		t.Fatalf("ReviewName: %v", err)
	}
	if reviewName != "gitea.example.com-acme--repo-pr-7" {
		t.Errorf("ReviewName = %q", reviewName)
	}
}

// runtime 引用校验与选择：main_agent/subagents/channels/provider 引用不存在
// 直接报错；多 runtime 必须指定；default_runtime 兜底。
func TestRuntimeSelection(t *testing.T) {
	base := func(runtimes string) string {
		return `{"channels": [{"type": "weixin", "bot_token": "t"}],
			"providers": {"gw": {}},
			"agents": {"qa": {"provider": "gw"}},
			"runtimes": ` + runtimes + `}`
	}
	valid := base(`{"review": {"main_agent": "qa", "subagents": ["ops"], "channels": ["weixin"], "provider": "gw"},
		"chat": {"channels": ["weixin"]}}`)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	runtime, err := file.ResolveRuntime("review")
	if err != nil {
		t.Fatalf("ResolveRuntime(review): %v", err)
	}
	if runtime.MainAgent != "qa" || !slices.Equal(runtime.Subagents, []string{"ops"}) {
		t.Errorf("runtime = %+v", runtime)
	}
	if _, err := file.ResolveRuntime(""); err == nil {
		t.Error("无 default_runtime 的多 runtime 配置应要求显式指定")
	}
	rejects := map[string]string{
		"unknown main agent": base(`{"main": {"main_agent": "nope"}}`),
		"unknown subagent":   base(`{"main": {"subagents": ["nope"]}}`),
		"unknown channel":    base(`{"main": {"channels": ["telegram"]}}`),
		"unknown provider":   base(`{"main": {"provider": "nope"}}`),
		"negative interval":  base(`{"main": {"interval_ms": -1}}`),
		"bad default":        base(`{"main": {}}`)[:0] + `{"channels": [{"type": "weixin", "bot_token": "t"}], "runtimes": {"main": {}}, "default_runtime": "nope"}`,
	}
	for name, bad := range rejects {
		t.Run(name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(badPath); err == nil {
				t.Errorf("Load 应拒绝：%s", bad)
			}
		})
	}
}
