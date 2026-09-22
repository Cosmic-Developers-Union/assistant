package instances

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	instance := file.Instances[0]
	if instance.Host != "https://gitea.example.com" {
		t.Errorf("Host = %q, want trailing slash trimmed", instance.Host)
	}
	if instance.Reviewer.Name != DefaultReviewerName || instance.Merger.Name != DefaultMergerName {
		t.Errorf("account defaults = %+v/%+v", instance.Reviewer, instance.Merger)
	}
	if len(instance.Repos) != 2 {
		t.Fatalf("Repos = %+v", instance.Repos)
	}
	if instance.Repos[0].Name != "owner/repo" || instance.Repos[0].Dir != "" {
		t.Errorf("Repos[0] = %+v", instance.Repos[0])
	}
	if instance.Repos[1].Name != "owner/another" || instance.Repos[1].Dir != "/srv/another" {
		t.Errorf("Repos[1] = %+v", instance.Repos[1])
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
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Instances[0].Reviewer.Name != "ai" || loaded.Instances[0].Merger.Name != "merge" {
		t.Errorf("account round-trip failed: %+v/%+v", loaded.Instances[0].Reviewer, loaded.Instances[0].Merger)
	}
	if len(loaded.Instances[0].Repos) != 2 ||
		loaded.Instances[0].Repos[0].Name != "owner/repo" ||
		loaded.Instances[0].Repos[1].Dir != "/srv/another" {
		t.Errorf("repo round-trip failed: %+v", loaded.Instances[0].Repos)
	}
	// 凭据已迁出配置：配置里不应再出现任何令牌字段。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
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

// 受管克隆/状态目录与 run 的当前目录解耦：落在数据目录下，按站点与仓库分层。
func TestDefaultManagedPaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	data := os.Getenv("XDG_DATA_HOME")

	repoDir, err := DefaultRepoDir("https://gitea.example.com", "acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "repos", "gitea.example.com", "acme", "repo")
	if repoDir != want {
		t.Errorf("DefaultRepoDir() = %q, want %q", repoDir, want)
	}
	stateDir, err := DefaultRepoStateDir("https://gitea.example.com:3000", "acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(data, "Cosmic-Developers-Union", "assistant", "state", "gitea.example.com-3000", "acme", "repo")
	if stateDir != want {
		t.Errorf("DefaultRepoStateDir() = %q, want %q", stateDir, want)
	}
	if _, err := DefaultRepoDir("https://gitea.example.com", "bad-name"); err == nil {
		t.Error("非法仓库名应报错")
	}
}

// 会话配置根由 assistant 托管（<配置目录>/claude），可用 CLAUDE_CONFIG_DIR 覆盖；
// 不再默认用户的 ~/.claude。
func TestClaudeDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	directory, err := ClaudeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "Cosmic-Developers-Union", "assistant", "claude")
	if directory != want {
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
	// 选择粒度：repo > instance > 全局默认
	if got := file.ProviderName(&file.Instances[0], &file.Instances[0].Repos[0]); got != "gateway" {
		t.Errorf("repo provider = %q, want gateway", got)
	}
	if got := file.ProviderName(&file.Instances[0], nil); got != "gateway" {
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
	// weixin.provider 生效：优先于全局默认
	file := &File{
		DefaultProvider: "gw",
		Weixin:          &Weixin{Provider: "weixin-gw"},
		Providers:       map[string]Provider{"gw": {}, "weixin-gw": {}},
	}
	if got := file.WeixinProviderName(); got != "weixin-gw" {
		t.Errorf("WeixinProviderName() = %q, want weixin-gw", got)
	}
	file.Weixin.Provider = ""
	if got := file.WeixinProviderName(); got != "gw" {
		t.Errorf("WeixinProviderName() fallback = %q, want gw", got)
	}
	if overrides := file.ProviderNameOf("gw").Overrides(); !overrides.Empty() {
		t.Errorf("空 provider 应为空覆盖：%+v", overrides)
	}
}

// 仓库自带的示例配置必须始终可加载（providers 等字段的回归护栏），并且示范
// $schema（编辑器补全的来源）。
func TestConfigExampleLoads(t *testing.T) {
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

// agent 池与 QQ 通道的加载、默认值与校验：引用不存在的 agent/provider 直接报错；
// qq.enabled 需要 app_id+app_secret；Normalize 填 api_base_url 缺省。
func TestLoadAgentsAndQQ(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"providers": {"zhipu": {"api_key": "k"}},
		"agents": {
			"ops": {"provider": "zhipu", "model": "glm-4.7", "system_prompt": "你是运维。", "session_timeout_ms": 60000},
			"coder": {"model": "m-coder"}
		},
		"default_agent": "ops",
		"qq": {"enabled": true, "app_id": "111", "app_secret": "sec", "admin_users": ["*"], "agent": "coder"},
		"weixin": {"enabled": true, "bot_token": "tok", "agent": "ops"}
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
	if file.QQ.APIBaseURL != DefaultQQAPIBaseURL {
		t.Errorf("qq.api_base_url 缺省未填：%q", file.QQ.APIBaseURL)
	}
	if file.Weixin.Agent != "ops" || file.QQ.Agent != "coder" {
		t.Errorf("通道 agent 未保留：weixin=%q qq=%q", file.Weixin.Agent, file.QQ.Agent)
	}

	rejects := map[string]string{
		"unknown default_agent":  `{"agents": {"ops": {}}, "default_agent": "nope", "instances": []}`,
		"unknown weixin.agent":   `{"agents": {"ops": {}}, "weixin": {"enabled": true, "bot_token": "t", "agent": "nope"}, "instances": []}`,
		"unknown qq.agent":       `{"agents": {"ops": {}}, "qq": {"enabled": true, "app_id": "1", "app_secret": "s", "agent": "nope"}, "instances": []}`,
		"unknown agent provider": `{"agents": {"ops": {"provider": "nope"}}, "instances": []}`,
		"qq enabled no secret":   `{"qq": {"enabled": true, "app_id": "111"}, "instances": []}`,
		"qq enabled no appid":    `{"qq": {"enabled": true, "app_secret": "s"}, "instances": []}`,
		"qq bad base url":        `{"qq": {"api_base_url": "api.sgroup.qq.com"}, "instances": []}`,
		"qq negative split":      `{"qq": {"enabled": true, "app_id": "1", "app_secret": "s", "split_limit": -5}, "instances": []}`,
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
		"bad type":        `{"channels": [{"type": "slack", "bot_token": "x"}]}`,
		"weixin no token": `{"channels": [{"type": "weixin"}]}`,
		"qq no secret":    `{"channels": [{"type": "qq", "app_id": "1"}]}`,
		"tg no token":     `{"channels": [{"type": "telegram"}]}`,
		"dup key":         `{"channels": [{"type": "weixin", "bot_token": "a"}, {"type": "weixin", "bot_token": "b"}]}`,
		"name with slash": `{"channels": [{"type": "weixin", "name": "a/b", "bot_token": "t"}]}`,
		"unnamed vs weixin block": `{"weixin": {"enabled": true, "bot_token": "t"}, "channels": [{"type": "weixin", "bot_token": "t2"}]}`,
		"unnamed vs qq block":     `{"qq": {"enabled": true, "app_id": "1", "app_secret": "s"}, "channels": [{"type": "qq", "app_id": "1", "app_secret": "s"}]}`,
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
	content := `{"agents": {"ops": {"mcp": {"search": {"command": "search-mcp", "args": ["-v"]}}}}, "qq": {"enabled": true, "app_id": "1", "app_secret": "s"}}`
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
