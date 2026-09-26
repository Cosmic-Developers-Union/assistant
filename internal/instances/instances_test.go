package instances

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/envref"
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
		"ANTHROPIC_API_KEY", "QQ_APP_ID", "QQ_TOKEN", "TELEGRAM_BOT_TOKEN", "SESSIONS_TOKEN",
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
		"bad type":          `{"channels": [{"type": "slack", "bot_token": "x"}]}`,
		"weixin no token":   `{"channels": [{"type": "weixin"}]}`,
		"qq no secret":      `{"channels": [{"type": "qq", "app_id": "1"}]}`,
		"tg no token":       `{"channels": [{"type": "telegram"}]}`,
		"dup key":           `{"channels": [{"type": "weixin", "bot_token": "a"}, {"type": "weixin", "bot_token": "b"}]}`,
		"name with slash":   `{"channels": [{"type": "weixin", "name": "a/b", "bot_token": "t"}]}`,
		"unknown agent ref": `{"channels": [{"type": "weixin", "bot_token": "t", "agent": "nope"}]}`,
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

// Notes 是载入规范化产生的备注出口（迁移与废弃提示）：无备注返回 nil（不是
// 空切片，调用方判空即可），有备注返回副本——调用方改不动内部的 notes。
func TestNotesAreNilWhenEmptyAndCopiedOtherwise(t *testing.T) {
	file := &File{}
	if got := file.Notes(); got != nil {
		t.Errorf("无备注应返回 nil：%v", got)
	}
	file.note("a %d", 1)
	file.note("b")
	notes := file.Notes()
	if !slices.Equal(notes, []string{"a 1", "b"}) {
		t.Fatalf("Notes = %v", notes)
	}
	notes[0] = "改坏了"
	if again := file.Notes(); again[0] != "a 1" {
		t.Errorf("Notes 必须返回副本：%v", again)
	}
}

// 仓库查找与清单：RepoNames 跳过空名（非 nil 入非 nil/空出空非 nil）；FindRepo
// 精确匹配（不做大小写或路径规范化），平台侧与仓库侧两个实现行为一致。
func TestRepoLookupAndNames(t *testing.T) {
	repos := []Repo{{Name: "acme/repo", Dir: "/srv/repo"}, {Name: ""}, {Name: "acme/other"}}
	instance := Instance{Repos: repos}
	if got := instance.RepoNames(); !slices.Equal(got, []string{"acme/repo", "acme/other"}) {
		t.Errorf("RepoNames = %v", got)
	}
	if got := (&Instance{}).RepoNames(); got == nil || len(got) != 0 {
		t.Errorf("无仓库应返回非 nil 空清单：%#v", got)
	}
	found, ok := instance.FindRepo("acme/other")
	if !ok || found.Dir != "" {
		t.Errorf("FindRepo = %+v, %v", found, ok)
	}
	if _, ok := instance.FindRepo("ACME/OTHER"); ok {
		t.Error("FindRepo 必须精确匹配（不做大小写折叠）")
	}
	if _, ok := instance.FindRepo("acme"); ok {
		t.Error("FindRepo 只认完整 owner/name")
	}

	channel := Channel{Type: ChannelGitea, Repos: repos}
	if found, ok := channel.FindRepo("acme/repo"); !ok || found.Dir != "/srv/repo" {
		t.Errorf("Channel.FindRepo = %+v, %v", found, ok)
	}
	if _, ok := (&Channel{}).FindRepo("acme/repo"); ok {
		t.Error("空通道不应命中")
	}
}

// provider 生效名的优先级链（repo > 通道 > runtime > 全局），空串表示内置缺省。
func TestGiteaProviderNamePrecedence(t *testing.T) {
	file := &File{DefaultProvider: "global"}
	runtime := Runtime{Provider: "runtime"}
	channel := &Channel{Provider: "channel"}
	repo := &Repo{Provider: "repo"}

	if got := file.GiteaProviderName(runtime, channel, repo); got != "repo" {
		t.Errorf("repo 优先级最高 = %q", got)
	}
	if got := file.GiteaProviderName(runtime, channel, &Repo{}); got != "channel" {
		t.Errorf("缺 repo.provider 应回退通道 = %q", got)
	}
	if got := file.GiteaProviderName(runtime, nil, nil); got != "runtime" {
		t.Errorf("缺通道应回退 runtime = %q", got)
	}
	if got := file.GiteaProviderName(Runtime{}, nil, nil); got != "global" {
		t.Errorf("都缺应回退全局缺省 = %q", got)
	}
	if got := (&File{}).GiteaProviderName(Runtime{}, &Channel{}, nil); got != "" {
		t.Errorf("无任何覆盖应为空串（内置缺省）= %q", got)
	}
}

// 配置定位是显式模式：全部锚定当前目录，不读平台配置目录。
func TestDefaultConfigPathFollowsWorkingDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	directory, err := DefaultConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if directory != work {
		t.Errorf("DefaultConfigDir() = %q, want %q", directory, work)
	}
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(work, "config.json"); path != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", path, want)
	}
}

// daemon 端点文件跟着配置文件走：ASSISTANT_CONFIG 指向别处时它随配置目录，
// 显式 --config 路径同理（不跟 cwd），空串回落当前目录。
func TestDaemonEndpointPaths(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("ASSISTANT_CONFIG", "")

	path, err := DaemonEndpointPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(work, "daemon.json"); path != want {
		t.Errorf("DaemonEndpointPath() = %q, want %q", path, want)
	}

	configDir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(configDir, "config.json"))
	path, err = DaemonEndpointPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configDir, "daemon.json"); path != want {
		t.Errorf("应跟随 ASSISTANT_CONFIG 目录：%q, want %q", path, want)
	}

	// 显式 --config 路径优先于环境变量；空串按未设置处理，回落 ASSISTANT_CONFIG
	// （不回落 cwd——daemon 端点始终跟着生效的配置文件）。
	explicitDir := t.TempDir()
	path, err = DaemonEndpointPathFor(filepath.Join(explicitDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(explicitDir, "daemon.json"); path != want {
		t.Errorf("DaemonEndpointPathFor() = %q, want %q", path, want)
	}
	path, err = DaemonEndpointPathFor("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configDir, "daemon.json"); path != want {
		t.Errorf("空 configPath 应回落 ASSISTANT_CONFIG：%q, want %q", path, want)
	}
}

// HostSlug 是受管目录名：省略 scheme、保留 host[:port]、其余字符折成 -；无
// scheme 时按 https 解析，不可解析（无 host）必须报错而不是产出坏目录名。
func TestHostSlugNormalization(t *testing.T) {
	accept := map[string]string{
		"https://gitea.example.com":       "gitea.example.com",
		"https://gitea.example.com/":      "gitea.example.com",
		"https://gitea.example.com/a/b":   "gitea.example.com",
		"gitea.example.com":               "gitea.example.com",
		"http://gitea.example.com:3000":   "gitea.example.com-3000",
		"https://GITEA.example.com":       "GITEA.example.com",
		"  https://gitea.example.com/  ":  "gitea.example.com",
		"https://user:pass@gitea.ex.co/x": "gitea.ex.co",
	}
	for host, want := range accept {
		t.Run(host, func(t *testing.T) {
			got, err := HostSlug(host)
			if err != nil {
				t.Fatalf("HostSlug(%q) error = %v", host, err)
			}
			if got != want {
				t.Errorf("HostSlug(%q) = %q, want %q", host, got, want)
			}
		})
	}
	for _, bad := range []string{"", "   ", "http://", "https:///path", "https://[::1"} {
		t.Run("reject "+bad, func(t *testing.T) {
			if got, err := HostSlug(bad); err == nil {
				t.Errorf("HostSlug(%q) 应报错，得到 %q", bad, got)
			} else if !strings.Contains(err.Error(), "无法解析平台地址") {
				t.Errorf("错误信息应说明无法解析：%v", err)
			}
		})
	}
}

// SaveBytes 的落盘契约：建父目录、0600、覆盖写（旧内容不残留）、同名冲突不
// 留下临时文件。
func TestSaveBytesWritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "config.json")
	content := []byte(`{"channels":[{"type":"weixin","bot_token":"t"}]}` + "\n")
	if err := SaveBytes(path, content); err != nil {
		t.Fatalf("SaveBytes: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("配置必须 0600：%v", mode)
	}
	read, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != string(content) {
		t.Errorf("写回内容不一致：%q", read)
	}

	// 覆盖写：新内容完整落盘，旧内容无残留（原子改名而非截断写）。
	replacement := []byte(`{"channels":[{"type":"qq","app_id":"1","app_secret":"s"}]}` + "\n")
	if err := SaveBytes(path, replacement); err != nil {
		t.Fatalf("SaveBytes 覆盖: %v", err)
	}
	read, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != string(replacement) {
		t.Errorf("覆盖写内容不一致：%q", read)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("不应留下临时文件：%v", entries)
	}
}

// Save 是纯序列化落盘（不做模型校验，写入前由调用方 Validate）：非法内容
// 也会写出文件——这里只锚定「写出的配置能再次 Load」的往返契约。
func TestSaveRoundTripsThroughLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	// 往返：Save 后 Load 得到等价配置（结构体 → JSON → 结构体不丢字段）。
	good := &File{
		DefaultProvider: "gateway",
		Providers: map[string]Provider{
			"gateway": {APIKey: "k", Env: map[string]string{"BASE_URL": "https://x"}},
		},
		Channels: []Channel{{Type: ChannelGitea, Host: "https://gitea.example.com", Repos: []Repo{{Name: "a/b"}}}},
		Runtimes: map[string]Runtime{"main": {Channels: []string{"gitea"}}},
	}
	if err := Save(path, good); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Save → Load 往返失败: %v", err)
	}
	if loaded.DefaultProvider != "gateway" || loaded.Providers["gateway"].APIKey != "k" {
		t.Errorf("往返丢字段：%+v", loaded)
	}
	if len(loaded.Channels) != 1 || loaded.Channels[0].Repos[0].Name != "a/b" {
		t.Errorf("通道往返不一致：%+v", loaded.Channels)
	}

	// 把关在 Load：Save 不校验（空配置也能写出文件），Load 才拒绝。
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := Save(empty, &File{}); err != nil {
		t.Fatalf("Save 不做校验：%v", err)
	}
	if _, err := Load(empty); err == nil {
		t.Error("Load 必须拒绝空配置")
	}
}

// 无 runtime 时 Load 合成 main；显式 gitea 通道已占用站点时 AddGiteaChannel
// 用 slug 命名（多站点登记不产生实例键冲突）。
func TestAddGiteaChannelNamesOnKeyCollision(t *testing.T) {
	// 首次追加：无人占用 "gitea" 键，保持未命名（老用户迁移无感）。
	file := &File{}
	file.AddGiteaChannel(Channel{Type: ChannelGitea, Host: "https://a.example.com"})
	if got := file.Channels[len(file.Channels)-1].Key(); got != "gitea" {
		t.Errorf("首次追加应保持未命名：%q", got)
	}
	// 再追加别的站点：与既有 "gitea" 键冲突，自动以 slug 命名。
	file.AddGiteaChannel(Channel{Type: ChannelGitea, Host: "https://b.example.com"})
	last := file.Channels[len(file.Channels)-1]
	if want := "gitea/b.example.com"; last.Key() != want {
		t.Errorf("冲突时应以 slug 命名：%q, want %q", last.Key(), want)
	}
	// 非 gitea 通道与已命名的 gitea 通道都不触发自动命名。
	named := &File{Channels: []Channel{{Type: ChannelGitea, Name: "mirror", Host: "https://m.example.com"}}}
	named.AddGiteaChannel(Channel{Type: ChannelGitea, Host: "https://b.example.com"})
	if got := named.Channels[1].Key(); got != "gitea" {
		t.Errorf("既有命名通道不占用 gitea 键：%q", got)
	}
	other := &File{}
	other.AddGiteaChannel(Channel{Type: ChannelWeixin, BotToken: "t"})
	if got := other.Channels[0].Key(); got != ChannelWeixin {
		t.Errorf("非 gitea 通道不受影响：%q", got)
	}
	// host 不可解析时无以命名，退回未命名（键冲突留给 Validate 报错）。
	broken := &File{}
	broken.AddGiteaChannel(Channel{Type: ChannelGitea, Host: "https://a.example.com"})
	broken.AddGiteaChannel(Channel{Type: ChannelGitea, Host: ":::"})
	if got := broken.Channels[1].Key(); got != "gitea" {
		t.Errorf("无法命名时应退回未命名：%q", got)
	}
}

// 遗留 instances 迁移：host 已被显式 gitea 通道占用时忽略并记备注；host 为
// 空的条目丢弃；迁移后备注可读、instances 清空（Save 不再落盘）。
func TestMigrateInstancesNotesAndDrops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"instances": [
			{"host": "https://covered.example.com", "repos": ["a/b"]},
			{"host": "https://kept.example.com", "reviewer": {"name": "bob"}, "repos": ["c/d"]}
		],
		"channels": [{"type": "gitea", "host": "https://covered.example.com"}]
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(file.Instances) != 0 {
		t.Errorf("instances 应迁移清空：%+v", file.Instances)
	}
	if len(file.Channels) != 2 {
		t.Fatalf("Channels = %+v", file.Channels)
	}
	if file.Channels[0].Host != "https://covered.example.com" {
		t.Errorf("显式通道应原样保留：%+v", file.Channels[0])
	}
	migrated := file.Channels[1]
	if migrated.Host != "https://kept.example.com" || migrated.Reviewer != "bob" {
		t.Errorf("迁移条目应带入 reviewer/repos：%+v", migrated)
	}
	if migrated.Merger != DefaultMergerName {
		t.Errorf("缺 merger 应用缺省账号：%q", migrated.Merger)
	}
	notes := file.Notes()
	if len(notes) != 2 {
		t.Fatalf("备注 = %v", notes)
	}
	if !strings.Contains(notes[0], "重复") || !strings.Contains(notes[1], "已迁移为 gitea 通道") {
		t.Errorf("备注内容不符合预期：%v", notes)
	}
}

// provider 的 env 清理：键名去空白、空键丢弃、值去空白；全空 env 折成 nil
// （避免写回配置里出现 "env":{}）；Settings/MCP 不在清理范围内（透传）。
func TestProviderNormalizedCleansEnv(t *testing.T) {
	provider := Provider{
		Env:      map[string]string{"  KEY  ": "  v  ", "  ": "drop", "\t": "drop"},
		Settings: map[string]any{"model": " m "},
		MCP:      map[string]any{"srv": "raw"},
	}
	normalized := provider.normalized()
	if keys := slices.Collect(maps.Keys(normalized.Env)); !slices.Equal(keys, []string{"KEY"}) {
		t.Errorf("env 键清理 = %v", normalized.Env)
	}
	if normalized.Env["KEY"] != "v" {
		t.Errorf("env 值去空白 = %q", normalized.Env["KEY"])
	}
	if fmt.Sprint(normalized.Settings["model"]) != " m " {
		t.Errorf("settings 应原样透传 = %#v", normalized.Settings)
	}
	if fmt.Sprint(normalized.MCP["srv"]) != "raw" {
		t.Errorf("mcp 应原样透传 = %#v", normalized.MCP)
	}
	if got := (Provider{Env: map[string]string{" ": "x"}}).normalized(); got.Env != nil {
		t.Errorf("全空 env 应折成 nil：%#v", got.Env)
	}
	if got := (Provider{}).normalized(); got.Env != nil {
		t.Errorf("空 provider 不应造出 env：%#v", got.Env)
	}
	// 幂等
	if again := normalized.normalized(); !slices.Equal(slices.Collect(maps.Keys(again.Env)), []string{"KEY"}) {
		t.Errorf("normalized 应幂等：%v", again.Env)
	}
}

// provider 反序列化的错误路径：非法 JSON、env 非对象、env 值非标量——都给出
// 指明字段的中文错误；未知键原样保留（读取-写回不丢）。
func TestProviderUnmarshalJSONRejectsMalformed(t *testing.T) {
	rejects := map[string]string{
		"not object":     `"just a string"`,
		"env not object": `{"env": "nope"}`,
		"env nested":     `{"env": {"NESTED": {"a": 1}}}`,
		"env array":      `{"env": {"LIST": [1, 2]}}`,
		"settings array": `{"settings": [1, 2]}`,
	}
	for name, bad := range rejects {
		t.Run(name, func(t *testing.T) {
			var provider Provider
			if err := json.Unmarshal([]byte(bad), &provider); err == nil {
				t.Errorf("应拒绝：%s", bad)
			}
		})
	}

	var provider Provider
	if err := json.Unmarshal([]byte(`{"env":{"N":3,"B":true},"unknown":{"keep":1}}`), &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Env["N"] != "3" || provider.Env["B"] != "true" {
		t.Errorf("标量 env 应折成字符串：%v", provider.Env)
	}
	encoded, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"unknown":{"keep":1}`) {
		t.Errorf("未知键应原样保留：%s", encoded)
	}
	// 空 provider 序列化为 {}（不写空 env/settings/mcp）
	encoded, err = json.Marshal(Provider{})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Errorf("空 provider = %s", encoded)
	}
}

// EffectiveOverrides 的错误路径：**未定义**的密钥引用必须在合并层之前被挡下
// （错误指出具体字段）。注意「已定义但为空」与「未定义」是两回事——前者是
// 显式配置的空值，照常放行；后者才是配置错误。
func TestEffectiveOverridesRejectsUndefinedRefs(t *testing.T) {
	const missing = "ASSISTANT_INSTANCES_MISSING"
	requireUnset(t, missing)
	file := &File{Providers: map[string]Provider{
		"broken": {APIKey: "${" + missing + "}"},
	}}
	_, err := file.EffectiveOverrides("broken")
	if err == nil {
		t.Fatal("未定义的密钥引用应报错")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("错误应指出字段：%v", err)
	}

	// 已定义但为空：显式空值不是配置错误，不得在此拦下
	t.Setenv(missing, "")
	emptyFile := &File{Providers: map[string]Provider{
		"empty": {APIKey: "${" + missing + "}"},
	}}
	if _, err := emptyFile.EffectiveOverrides("empty"); err != nil {
		t.Errorf("已定义的空值不应报错：%v", err)
	}

	t.Setenv("ASSISTANT_INSTANCES_SET", "value")
	file = &File{Providers: map[string]Provider{
		"ok": {APIKey: "${ASSISTANT_INSTANCES_SET}"},
	}}
	overrides, err := file.EffectiveOverrides("ok")
	if err != nil {
		t.Fatalf("已定义引用应可展开：%v", err)
	}
	if len(overrides.Env) == 0 {
		t.Errorf("预设展开应产出 env：%+v", overrides)
	}

	// 未知 provider 名：没有预设，与纯手写配置一致（不报错）。
	if _, err := (&File{}).EffectiveOverrides("no-such-provider"); err != nil {
		t.Errorf("未知 provider 名不应报错：%v", err)
	}
}

// Validate 的空配置与 runtimes 把关：至少一项、空 runtime 名、channels 引用
// 空键——都必须在写入前拒绝。
func TestValidateRejectsEmptyAndBadRuntimeRefs(t *testing.T) {
	cases := map[string]struct {
		file File
		want string
	}{
		"empty": {
			file: File{},
			want: "配置为空",
		},
		"blank channel ref": {
			file: File{Channels: []Channel{{Type: ChannelWeixin, BotToken: "t"}},
				Runtimes: map[string]Runtime{"main": {Channels: []string{""}}}},
			want: "空通道键",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			err := testCase.file.Validate()
			if err == nil {
				t.Fatal("Validate 应报错")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("错误 = %v, want 含 %q", err, testCase.want)
			}
		})
	}
}

// ResolveRuntime 缺省合成：没有配置任何 runtime 时即时合成 main（全部通道
// 键），不落盘；多 runtime 未指定时列出可用名。
func TestResolveRuntimeSynthesizesMain(t *testing.T) {
	file := &File{Channels: []Channel{
		{Type: ChannelWeixin, BotToken: "t"},
		{Type: ChannelQQ, AppID: "1", AppSecret: "s"},
	}}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if !slices.Equal(runtime.Channels, []string{ChannelWeixin, ChannelQQ}) {
		t.Errorf("合成 runtime 应含全部通道：%v", runtime.Channels)
	}

	multi := &File{Runtimes: map[string]Runtime{"a": {}, "b": {}}}
	_, err = multi.ResolveRuntime("")
	if err == nil || !strings.Contains(err.Error(), "需要 --runtime") {
		t.Errorf("多 runtime 未指定应报错并给提示：%v", err)
	}
	_, err = file.ResolveRuntime("nope")
	if err == nil || !strings.Contains(err.Error(), "未定义") {
		t.Errorf("未定义 runtime 应报错：%v", err)
	}
}

// requireUnset 确保变量在测试进程里确实未定义（t.Setenv 只能设值，不能清除）。
func requireUnset(t *testing.T, name string) {
	t.Helper()
	if value, ok := os.LookupEnv(name); ok {
		t.Skipf("%s 已在环境里定义为 %q，无法测未定义分支", name, value)
	}
}
