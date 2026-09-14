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
      "admin_token": "admin",
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
		"oauth without refresh": `{"instances": [{"host": "https://a.example.com", "repos": [],
			"admin_oauth": {"client_id": "c1"}}]}`,
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
		Host:       "https://gitea.example.com",
		AdminToken: "admin",
		AdminOAuth: &OAuthCredential{ClientID: "client-1", ClientSecret: "secret-1", RefreshToken: "refresh-1"},
		Reviewer:   Account{Name: "ai", Token: "reviewer-token"},
		Merger:     Account{Name: "merge", Token: "merger-token"},
		Repos:      []Repo{{Name: "owner/repo", MergerToken: "repo-merger-token"}, {Name: "owner/another", Dir: "/srv/another"}},
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
	if loaded.Instances[0].Reviewer.Token != "reviewer-token" {
		t.Errorf("token round-trip failed: %+v", loaded.Instances[0].Reviewer)
	}
	if loaded.Instances[0].AdminOAuth == nil || loaded.Instances[0].AdminOAuth.RefreshToken != "refresh-1" {
		t.Errorf("admin_oauth round-trip failed: %+v", loaded.Instances[0].AdminOAuth)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"repo-merger-token"`) || !strings.Contains(string(raw), `"merger_token"`) {
		t.Errorf("repo merger token should be persisted:\n%s", raw)
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
	if err := json.Unmarshal([]byte(`{"name":"owner/repo","dir":"/srv/repo","merger_token":"tok-1"}`), &object); err != nil {
		t.Fatal(err)
	}
	if object.Dir != "/srv/repo" || object.MergerToken != "tok-1" {
		t.Errorf("object = %+v", object)
	}
	// 有 dir/merger_token 时序列化为对象（不丢字段）
	encoded, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"merger_token":"tok-1"`) {
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
	if overrides := file.LookupProvider("gw").Overrides(); !overrides.Empty() {
		t.Errorf("空 provider 应为空覆盖：%+v", overrides)
	}
}

// 仓库自带的示例配置必须始终可加载（providers 等字段的回归护栏）。
func TestConfigExampleLoads(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config.example.json")); err != nil {
		t.Fatalf("config.example.json 无法加载：%v", err)
	}
}
