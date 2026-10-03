package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/weixin"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
	"github.com/spf13/cobra"
)

func command(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewRootCommand(&out, &errOut)
	root.SetArgs(args)
	root.SetIn(strings.NewReader(""))
	err := root.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}
func TestCommandSurfaces(t *testing.T) {
	out, _, err := command(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"instance", "action", "mcp", "run"} {
		if !strings.Contains(out, word) {
			t.Fatal(out)
		}
	}
	for _, word := range []string{"login", "setup", "init", "doctor", "validate", "config"} {
		root := NewRootCommand(&bytes.Buffer{}, &bytes.Buffer{})
		if cmd, _, err := root.Find([]string{word}); err == nil && cmd.Name() == word {
			t.Fatal("旧入口仍暴露", word)
		}
	}
	for _, args := range [][]string{{"instance", "add", "gitea", "--password", "secret"}, {"run", "--runtime", "old"}, {"action", "label-sync", "--repo", "acme/repo"}, {"action", "label-sync", "unexpected"}} {
		if _, _, err := command(t, args...); err == nil {
			t.Fatal(args)
		}
	}
	if _, _, err := command(t, "run", "--config", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("run 默默回退旧登录态")
	}
}
func TestInstanceListShowRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	out, _, err := command(t, "instance", "list")
	if err != nil || !strings.Contains(out, "尚未") {
		t.Fatal(out, err)
	}
	file := &credentials.File{Instances: credentials.Instances{Gitea: []credentials.Gitea{{Name: "site", URL: "https://site", Username: "dev", Token: "secret"}}, QQ: []credentials.QQ{{Name: "qq", AppID: "1", AppSecret: "secret"}}, Weixin: []credentials.Weixin{{Name: "wx", URL: "https://site", UserID: "u", BotID: "b", Token: "secret"}}, Telegram: []credentials.Telegram{{Name: "tg", Username: "bot", Token: "secret"}}}}
	if err := credentials.Save(path, file); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"site", "qq", "wx", "tg"} {
		out, _, err := command(t, "instance", "show", name)
		if err != nil || !strings.Contains(out, name) || strings.Contains(out, "secret") {
			t.Fatal(out, err)
		}
	}
	out, _, err = command(t, "instance", "list")
	if err != nil || strings.Contains(out, "secret") {
		t.Fatal(out, err)
	}
	for _, name := range []string{"site", "qq", "wx", "tg"} {
		if _, _, err := command(t, "instance", "remove", name); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range []string{"show", "remove"} {
		if _, _, err := command(t, "instance", operation, "missing"); err == nil {
			t.Fatal(operation)
		}
	}
	_ = os.WriteFile(path, []byte("corrupt"), 0o600)
	for _, op := range []string{"list", "show", "remove"} {
		args := []string{"instance", op}
		if op != "list" {
			args = append(args, "name")
		}
		if _, _, err := command(t, args...); err == nil {
			t.Fatal("损坏凭据未报告")
		}
	}
}
func TestGiteaInstanceLoginUsesSDKAndDoesNotPersistPassword(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		user, password, ok := r.BasicAuth()
		if !ok || user != "dev" || password != "password-only-for-login" {
			http.Error(w, "bad auth", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/user":
			fmt.Fprint(w, `{"login":"dev"}`)
		case "/api/v1/users/dev/tokens":
			fmt.Fprint(w, `{"sha1":"issued-token"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	passwordFile := filepath.Join(t.TempDir(), "password")
	_ = os.WriteFile(passwordFile, []byte("password-only-for-login\n"), 0o600)
	_, _, err := command(t, "instance", "add", "gitea", "--name", "site", "--url", server.URL, "--username", "dev", "--password-file", passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "password-only") {
		t.Fatal("密码落盘")
	}
	file, err := credentials.Load(path)
	if err != nil || file.Instances.Gitea[0].Token != "issued-token" {
		t.Fatal(err, string(data))
	}
	if len(requests) != 2 {
		t.Fatal(requests)
	}
	if _, _, err := command(t, "instance", "add", "gitea", "--name", "site"); err == nil {
		t.Fatal("重复实例触发登录")
	}
	if _, _, err := command(t, "instance", "add", "gitea", "--name", "missing"); err == nil {
		t.Fatal("非交互输入没有拒绝缺字段")
	}
	if _, err := loginGitea(t.Context(), server.URL, "other", "wrong", "", "test"); err == nil {
		t.Fatal("错误密码被接受")
	}
}
func TestActionRequiresExplicitEnvironmentBeforeAccess(t *testing.T) {
	t.Setenv("GITEA_HOST", "")
	t.Setenv("GITEA_ACCESS_TOKEN", "")
	t.Setenv("GITEA_REPOSITORY", "")
	for _, op := range []string{"label-sync", "automerge"} {
		if _, _, err := command(t, "action", op); err == nil {
			t.Fatal("action 未限定仓库")
		}
	}
	t.Setenv("GITEA_HOST", "https://site")
	t.Setenv("GITEA_ACCESS_TOKEN", "token")
	t.Setenv("GITEA_REPOSITORY", "invalid")
	if _, _, err := command(t, "action", "automerge"); err == nil {
		t.Fatal("坏仓库名未拒绝")
	}
}
func TestPromptSecretAndValidation(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	p := newPrompts(cmd)
	if _, err := p.value("", "账号"); err == nil {
		t.Fatal("非交互进入询问")
	}
	if value, err := p.value("given", "账号"); err != nil || value != "given" {
		t.Fatal(err)
	}
	if _, err := p.secret("", "密码"); err == nil {
		t.Fatal("非交互询问密码")
	}
	if _, err := p.secret(filepath.Join(t.TempDir(), "missing"), "密码"); err == nil {
		t.Fatal("文件读取错误被吞掉")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, err := p.secret(empty, "密码"); err == nil {
		t.Fatal("空密码被接受")
	}
	if _, err := loginGitea(context.Background(), "https://%bad", "u", "s", "", "name"); err == nil {
		t.Fatal("坏地址被接受")
	}
}

func TestInjectedPlatformLoginContracts(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secretFile, []byte("secret"), 0o600)
	for _, kind := range []string{"qq", "telegram", "weixin"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, fail), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "credentials.json")
				t.Setenv("ASSISTANT_CREDENTIALS", path)
				failure := fmt.Errorf("平台拒绝")
				deps := defaultInstanceLogins()
				deps.QQ = func(ctx context.Context, id, secret string) error {
					if id != "id" || secret != "secret" {
						t.Fatal("QQ 字段混用")
					}
					if fail {
						return failure
					}
					return nil
				}
				deps.Telegram = func(ctx context.Context, token string) (string, error) {
					if fail {
						return "", failure
					}
					return "bot", nil
				}
				deps.Weixin = func(ctx context.Context, url string, onQR func(weixin.QRCode), verify func(int) (string, error)) (weixin.Credentials, error) {
					onQR(weixin.QRCode{Content: ""})
					if _, err := verify(1); err == nil {
						t.Fatal("非 TTY 接受验证码")
					}
					if fail {
						return weixin.Credentials{}, failure
					}
					return weixin.Credentials{BotToken: "secret", BaseURL: "https://site", BotID: "b", UserID: "u"}, nil
				}
				cmd := newInstanceCommandWithLogins(deps)
				cmd.SetIn(strings.NewReader(""))
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				args := []string{"add", kind, "--name", kind}
				if kind == "qq" {
					args = append(args, "--app-id", "id", "--app-secret-file", secretFile)
				}
				if kind == "telegram" {
					args = append(args, "--token-file", secretFile)
				}
				cmd.SetArgs(args)
				err := cmd.ExecuteContext(t.Context())
				if fail {
					if err == nil {
						t.Fatal("失败登录落盘")
					}
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("失败改变了凭据")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	// 缺失字段与密钥文件在联网前就拒绝。
	for _, args := range [][]string{{"add", "qq", "--name", "qq"}, {"add", "qq", "--name", "qq", "--app-id", "id", "--app-secret-file", "/missing"}, {"add", "telegram", "--name", "tg", "--token-file", "/missing"}, {"add", "gitea", "--name", "site", "--url", "bad"}, {"add", "gitea", "--name", "site", "--url", "https://site"}, {"add", "gitea", "--name", "site", "--url", "https://site", "--username", "dev", "--password-file", "/missing"}} {
		t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
		if _, _, err := command(t, append([]string{"instance"}, args...)...); err == nil {
			t.Fatal(args)
		}
	}
}

func TestActionUsesOnlyTargetAndDryRunIsReadOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("演练写入平台: %s %s", r.Method, r.URL)
			http.Error(w, "write", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			fmt.Fprint(w, `{"version":"1.27.0"}`)
		case "/api/v1/user":
			fmt.Fprint(w, `{"login":"merge"}`)
		case "/api/v1/repos/acme/repo/labels", "/api/v1/repos/acme/repo/pulls", "/api/v1/repos/acme/repo/issues", "/api/v1/repos/acme/repo/branch_protections":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("命令访问其他仓库或枚举全站: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("GITEA_HOST", server.URL)
	t.Setenv("GITEA_ACCESS_TOKEN", "token")
	t.Setenv("GITEA_REPOSITORY", "acme/repo")
	for _, kind := range []string{"label-sync", "automerge"} {
		if _, _, err := command(t, "action", kind, "--dry-run", "--verbose"); err != nil {
			t.Fatal(err)
		}
	}
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" {
			fmt.Fprint(w, `{"version":"1.27.0"}`)
			return
		}
		http.Error(w, "denied", 401)
	}))
	defer denied.Close()
	t.Setenv("GITEA_HOST", denied.URL)
	if _, _, err := command(t, "action", "label-sync"); err == nil {
		t.Fatal("认证失败被吞掉")
	}
}

func TestExecuteExitCodes(t *testing.T) {
	for _, entry := range []struct {
		err  error
		code int
	}{{nil, 0}, {fmt.Errorf("失败"), 1}, {&status.FatalError{Reason: "凭据失败"}, 78}, {fmt.Errorf("取消输入: %w", context.Canceled), 130}} {
		cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return entry.err }, SilenceErrors: true, SilenceUsage: true}
		cmd.SetArgs([]string{})
		if code := Execute(cmd); code != entry.code {
			t.Fatal(code, entry.code)
		}
	}
}

func TestRunAndMCPCommandWiring(t *testing.T) {
	rootDir := t.TempDir()
	path := filepath.Join(rootDir, "config.yaml")
	t.Setenv("TOKEN", "token")
	data := "connects:\n  tg: {type: telegram, token: '{{TOKEN}}'}\nbots:\n  chat: {kind: chat, use: {telegram: tg}, workspace: {type: directory}}\n"
	_ = os.WriteFile(path, []byte(data), 0o600)
	if _, _, err := command(t, "run", "--config", path, "--dry-run", "--debug"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := command(t, "mcp", "sessions", "--config", path); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(rootDir, "mcp")
	_ = os.WriteFile(script, []byte("#!/bin/sh\necho connected\n"), 0o700)
	t.Setenv("GITEA_MCP_BIN", script)
	out, _, err := command(t, "mcp", "gitea", "--host", "https://site", "--token", "token", "--verbose")
	if err != nil || !strings.Contains(out, "connected") {
		t.Fatal(out, err)
	}
	deps := defaultInstanceLogins()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = deps.QQ(ctx, "id", "secret")
	_, _ = deps.Telegram(ctx, "token")
	_, _ = deps.Weixin(ctx, "", nil, nil)
}

func TestLoginRejectsWrongIdentityAndTokenFailure(t *testing.T) {
	for _, mode := range []string{"wrong-identity", "token-error"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/user" {
				if mode == "wrong-identity" {
					fmt.Fprint(w, `{"login":"other"}`)
				} else {
					fmt.Fprint(w, `{"login":"dev"}`)
				}
				return
			}
			http.Error(w, "denied", 403)
		}))
		if _, err := loginGitea(t.Context(), server.URL, "dev", "secret", "", "site"); err == nil {
			t.Fatal(mode)
		}
		server.Close()
	}
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte("secret"), 0o600)
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	deps := defaultInstanceLogins()
	deps.Telegram = func(context.Context, string) (string, error) { _ = os.Mkdir(path, 0o700); return "bot", nil }
	cmd := newInstanceCommandWithLogins(deps)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"add", "telegram", "--name", "tg", "--token-file", secret})
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "保存") {
		t.Fatal("平台登录成功后写盘失败未解释", err)
	}
}
