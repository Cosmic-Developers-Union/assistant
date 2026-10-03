package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `connects:
  site: {type: gitea, url: https://gitea.example, token: "{{TOKEN}}"}
mcp:
  site: {cmd: assistant, args: [mcp, gitea], env: {CUSTOM: "{{TOKEN}}"}}
bots:
  review:
    kind: gitea-review
    use: {gitea: site}
    with: {identity: ai}
    workspace: {type: worktree, repo: "{{event.repo}}", ref: "{{event.pr.ref}}"}
    agent: {mcp: [site]}
`

func lookup(key string) (string, bool) { return "private-value", key == "TOKEN" }
func TestDecodeConfig(t *testing.T) {
	cfg, err := Decode([]byte(validConfig), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connects["site"].Token != "private-value" || cfg.Runtime.Interval != 30*time.Second || cfg.Session.Store != "local" {
		t.Fatalf("配置未正确展开或补缺省: %+v", cfg.Runtime)
	}
	if cfg.Bots["review"].Workspace.Ref != "{{event.pr.ref}}" {
		t.Fatal("事件模板被提前展开")
	}
	for name, data := range map[string]string{
		"unknown-top":          validConfig + "channels: []\n",
		"unknown-connect":      strings.Replace(validConfig, "type: gitea", "typo: gitea", 1),
		"unknown-bot":          strings.Replace(validConfig, "kind: gitea-review", "kind: gitea-review\n    typo: x", 1),
		"multiple-documents":   validConfig + "---\n{}\n",
		"duplicate":            validConfig + "bots: {}\n",
		"missing-bots":         "connects: {}\n",
		"plaintext-secret":     strings.Replace(validConfig, "{{TOKEN}}", "secret", 1),
		"secret-suffix":        strings.Replace(validConfig, "{{TOKEN}}", "{{TOKEN}}suffix", 1),
		"missing-env":          strings.ReplaceAll(validConfig, "TOKEN", "MISSING"),
		"bad-url":              strings.Replace(validConfig, "https://gitea.example", "https://user:secret@gitea.example", 1),
		"removed-repo-filter":  strings.Replace(validConfig, "type: gitea,", "type: gitea, repos: [acme/repo],", 1),
		"bad-repo":             strings.Replace(validConfig, "{{event.repo}}", "../repo", 1),
		"cross-fields":         strings.Replace(validConfig, "type: gitea,", "type: gitea, app-id: 1,", 1),
		"missing-connection":   strings.Replace(validConfig, "gitea: site", "gitea: missing", 1),
		"wrong-role":           strings.Replace(validConfig, "gitea: site", "qq: site", 1),
		"multiple-connections": strings.Replace(validConfig, "gitea: site", "gitea: site, qq: site", 1),
		"unknown-kind":         strings.Replace(validConfig, "kind: gitea-review", "kind: bad", 1),
		"missing-mcp":          strings.Replace(validConfig, "mcp: [site]", "mcp: [missing]", 1),
		"missing-cmd":          strings.Replace(validConfig, "cmd: assistant", "cmd: ''", 1),
		"missing-workspace":    strings.Replace(validConfig, "type: worktree", "type: ''", 1),
		"bad-template":         strings.Replace(validConfig, "event.repo", "event.unknown", 1),
		"bad-ref":              strings.Replace(validConfig, "{{event.pr.ref}}", "--upload-pack=bad", 1),
		"missing-repo":         strings.Replace(validConfig, "repo: \"{{event.repo}}\"", "repo: ''", 1),
		"local-s3":             validConfig + "session: {store: local, bucket: bucket}\n",
		"bad-store":            validConfig + "session: {store: none}\n",
		"s3-missing":           validConfig + "session: {store: s3}\n",
		"interval":             validConfig + "runtime: {interval: -1s}\n",
		"timeout":              validConfig + "runtime: {timeout: -1s}\n",
		"capacity":             validConfig + "runtime: {concurrency: -1}\n",
		"invalid-env":          strings.Replace(validConfig, "CUSTOM:", "BAD-NAME:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(data), lookup)
			if err == nil {
				t.Fatal("非法配置被接受")
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("错误泄露密钥")
			}
		})
	}
}
func TestPlatformConfig(t *testing.T) {
	for _, platform := range []string{"qq", "weixin", "telegram"} {
		fields := "token: '{{TOKEN}}'"
		if platform == "qq" {
			fields = "app-id: '1', app-secret: '{{TOKEN}}'"
		}
		data := "connects:\n  chat: {type: " + platform + ", " + fields + "}\nbots:\n  chat: {kind: chat, use: {" + platform + ": chat}, workspace: {type: directory}}\n"
		cfg, err := Decode([]byte(data), lookup)
		if err != nil {
			t.Fatal(err)
		}
		conn := cfg.Connects["chat"]
		conn.AppID = "混入其他平台字段"
		if platform == "qq" {
			conn.UserID = "unexpected"
		}
		cfg.Connects["chat"] = conn
		if err := cfg.Validate(); err == nil {
			t.Fatal("消息平台接受仓库字段")
		}
	}
	cfg, _ := Decode([]byte(validConfig), lookup)
	cfg.Bots["copy"] = cfg.Bots["review"]
	if err := cfg.Validate(); err == nil {
		t.Fatal("重复消费者被接受")
	}
	cfg, _ = Decode([]byte(validConfig), lookup)
	conn := cfg.Connects["site"]
	conn.Type = "unknown"
	cfg.Connects["site"] = conn
	if err := cfg.Validate(); err == nil {
		t.Fatal("未知平台被接受")
	}
	data := validConfig + "session: {store: s3, endpoint: https://minio.example, bucket: sessions, access-key: '{{TOKEN}}', secret-key: '{{TOKEN}}'}\n"
	if _, err := Decode([]byte(data), lookup); err != nil {
		t.Fatal(err)
	}
}
func TestLoadRootsAndReferenceErrors(t *testing.T) {
	t.Setenv("TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Root != filepath.Join(filepath.Dir(path), "data") {
		t.Fatal(cfg.Runtime.Root)
	}
	if _, err := Load(path + ".missing"); err == nil {
		t.Fatal("缺失配置被接受")
	}
	for _, value := range []string{"{{MISSING}}", "{{TOKEN", "{{event.repo}}", "{{1BAD}}", "{{TOKEN}}}}"} {
		if _, err := ExpandEnv(value, lookup); err == nil {
			t.Errorf("%q 没有报错", value)
		}
	}
	if got, err := ExpandEnv("plain {{ TOKEN }}", lookup); err != nil || got != "plain private-value" {
		t.Fatalf("%q %v", got, err)
	}
	if got := eventTemplate("{{ event.repo }} {{event.pr.head}} {{event.base}}", Event{Repo: "acme/repo", Head: "head", Base: "main"}); got != "acme/repo head main" {
		t.Fatal(got)
	}
}
