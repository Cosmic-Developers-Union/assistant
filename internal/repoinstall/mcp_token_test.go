package repoinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
	"assistant/internal/instances"
)

// 凭据库优先于旧 config.json 字段：同站点两条来源都在时用凭据库那条，凭据库没有
// 时才回退旧字段（迁移期兼容）。
func TestMCPTokenPrefersCredentialsStore(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", MCPToken: "legacy-a"},
		{Host: "https://b.example.com", MCPToken: "legacy-b"},
	}}); err != nil {
		t.Fatal(err)
	}
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: "https://a.example.com", User: "alice", Purpose: credentials.PurposeMCP, Token: "store-a",
	})
	credentialPath := filepath.Join(dir, "credentials.json")
	if err := credentials.Save(credentialPath, store); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "ASSISTANT_CONFIG" {
			return configPath
		}
		return ""
	}
	token, source, err := resolveMCPToken("https://a.example.com", "", getenv)
	if err != nil || token != "store-a" || !strings.Contains(source, "@alice") {
		t.Fatalf("应优先用凭据库：token=%q source=%q err=%v", token, source, err)
	}
	token, source, err = resolveMCPToken("https://b.example.com", "", getenv)
	if err != nil || token != "legacy-b" || !strings.Contains(source, "旧版字段") {
		t.Fatalf("凭据库缺失时应回退旧字段：token=%q source=%q err=%v", token, source, err)
	}
	// ASSISTANT_CREDENTIALS 显式指定凭据库位置
	if token, _, err := resolveMCPToken("https://a.example.com", "", func(key string) string {
		if key == "ASSISTANT_CREDENTIALS" {
			return credentialPath
		}
		return ""
	}); err != nil || token != "store-a" {
		t.Fatalf("ASSISTANT_CREDENTIALS 覆盖失败：token=%q err=%v", token, err)
	}
}

func TestMCPTokenIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "instances.json")
	teaPath := filepath.Join(dir, "tea.yml")
	if err := instances.Save(path, &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", MCPToken: "token-a"},
		{Host: "https://b.example.com", MCPToken: "token-b"},
		{Host: "https://admin.example.com", AdminToken: "admin-only"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(teaPath, []byte("logins:\n- url: https://a.example.com\n  token: tea-a\n- url: https://c.example.com\n  token: tea-c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"Cosmic-Developers-Union/assistant/token", "mmc/gitea-token"} {
		p := filepath.Join(dir, legacy)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("legacy-token"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ host, want string }{
		{"https://a.example.com/", "token-a"},
		{"https://b.example.com", "token-b"},
		{"https://c.example.com", ""},
		{"https://unknown.example.com", ""},
		{"https://admin.example.com", ""},
	} {
		t.Run(tc.host, func(t *testing.T) {
			getenv := func(key string) string {
				return map[string]string{"ASSISTANT_CONFIG": path, "TEA_CONFIG": teaPath}[key]
			}
			token, _, err := resolveMCPToken(tc.host, "", getenv)
			if err != nil || token != tc.want {
				t.Fatalf("token = %q, err = %v; want %q", token, err, tc.want)
			}
			if tc.want == "" {
				_, err := ResolveMCP(context.Background(), MCPOptions{Host: tc.host, Getenv: getenv})
				if err == nil || !strings.Contains(err.Error(), "assistant login "+tc.host) {
					t.Fatalf("expected host-specific login guidance, got %v", err)
				}
			}
		})
	}
}

func TestMCPTokenExplicitOverrides(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(" file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, envToken, file, want string
		wantErr                    bool
	}{
		{"env first", "env-token", "/missing", "env-token", false},
		{"file", "", file, "file-token", false},
		{"missing file fails", "", file + ".missing", "", true},
		{"invalid config fails", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				return map[string]string{"GITEA_ACCESS_TOKEN": tc.envToken, "GITEA_ACCESS_TOKEN_FILE": tc.file}[key]
			}
			token, _, err := resolveMCPToken("https://a.example.com", file+".missing", getenv)
			if (err != nil) != tc.wantErr || token != tc.want {
				t.Fatalf("token = %q, err = %v", token, err)
			}
		})
	}
}
