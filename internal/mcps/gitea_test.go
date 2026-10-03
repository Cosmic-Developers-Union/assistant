package mcps

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

func TestResolutionAndStrictAmbiguity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	file := &credentials.File{Instances: credentials.Instances{Gitea: []credentials.Gitea{{Name: "site", URL: "https://site/", Username: "dev", Token: "stored"}}}}
	if err := file.Instances.Gitea[0].SetPassword("login-secret"); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Save(path, file); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	options := GiteaOptions{Dir: t.TempDir(), Getenv: getenv, Probe: func(string) bool { return false }}
	spec, err := ResolveGitea(t.Context(), options)
	if err != nil || spec.Host != "https://site" || spec.Token != "stored" {
		t.Fatal(spec, err)
	}
	options.Host = "https://override/"
	options.Token = "explicit"
	spec, err = ResolveGitea(t.Context(), options)
	if err != nil || spec.Host != "https://override" || spec.Token != "explicit" {
		t.Fatal(spec, err)
	}
	options.Host = ""
	options.Token = ""
	options.Getenv = func(key string) string {
		if key == "GITEA_HOST" {
			return "https://env"
		}
		if key == "GITEA_ACCESS_TOKEN" {
			return "env-token"
		}
		return ""
	}
	spec, err = ResolveGitea(t.Context(), options)
	if err != nil || spec.Token != "env-token" {
		t.Fatal(err)
	}
	file.Instances.Gitea = append(file.Instances.Gitea, credentials.Gitea{Name: "other", URL: "https://other", Username: "other", Token: "other"})
	_ = credentials.Save(path, file)
	options.Getenv = getenv
	if _, err := ResolveGitea(t.Context(), options); err == nil {
		t.Fatal("无 remote 多站点猜测")
	}
	options.Host = "https://missing"
	if _, err := ResolveGitea(t.Context(), options); err == nil || !strings.Contains(err.Error(), "instance add") {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("file-token\n"), 0o600)
	options.Getenv = func(key string) string {
		if key == "GITEA_ACCESS_TOKEN_FILE" {
			return tokenFile
		}
		return ""
	}
	spec, err = ResolveGitea(t.Context(), options)
	if err != nil || spec.Token != "file-token" {
		t.Fatal(err)
	}
	_ = os.WriteFile(tokenFile, nil, 0o600)
	if _, err := ResolveGitea(t.Context(), options); err == nil {
		t.Fatal("空覆盖回退凭据")
	}
	_ = os.Remove(tokenFile)
	if _, err := ResolveGitea(t.Context(), options); err == nil {
		t.Fatal("缺失覆盖回退凭据")
	}
	_ = os.WriteFile(path, []byte("bad"), 0o600)
	options.Host = ""
	options.Getenv = getenv
	if _, err := ResolveGitea(t.Context(), options); err == nil {
		t.Fatal("坏凭据被吞掉")
	}
	_ = os.Remove(path)
	if _, err := ResolveGitea(t.Context(), options); err == nil {
		t.Fatal("无站点启动 MCP")
	}
}
func TestMCPCommandAndExitPropagation(t *testing.T) {
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }
	bin, args := giteaCommand(getenv)
	if bin != "go" || args[1] != DefaultGiteaModule {
		t.Fatal(bin, args)
	}
	env["GITEA_MCP_MODULE"] = "example@v1"
	_, args = giteaCommand(getenv)
	if args[1] != "example@v1" {
		t.Fatal(args)
	}
	script := filepath.Join(t.TempDir(), "mcp")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$GITEA_HOST:$GITEA_ACCESS_TOKEN\"\nexit 7\n"), 0o700)
	env["GITEA_MCP_BIN"] = script
	env["GITEA_MCP_SCOPES"] = "issue"
	bin, args = giteaCommand(getenv)
	if bin != script || args[3] != "issue" {
		t.Fatal(bin, args)
	}
	var out bytes.Buffer
	code := 0
	err := RunGitea(t.Context(), GiteaOptions{Host: "https://site", Token: "private-token", Getenv: getenv, Dir: t.TempDir(), Stdout: &out, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader(""), Exit: func(value int) { code = value }, Log: func(string, ...any) {}})
	if err != nil || code != 7 || out.String() != "https://site:private-token" {
		t.Fatal(out.String(), code, err)
	}
	env["GITEA_MCP_BIN"] = "/missing/mcp"
	if err := RunGitea(context.Background(), GiteaOptions{Host: "https://site", Token: "token", Getenv: getenv}); err == nil {
		t.Fatal("启动错误被吞掉")
	}
	if maskGiteaToken("short") != "***" || strings.Contains(maskGiteaToken("private-token"), "private-token") {
		t.Fatal("密钥未打码")
	}
}

func TestExplicitInstanceSelectsAccountWithoutGuessingOrEnvironmentFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	file := &credentials.File{Instances: credentials.Instances{Gitea: []credentials.Gitea{{Name: "ai", URL: "https://site", Username: "ai", Token: "review-token"}, {Name: "merge", URL: "https://site", Username: "merge", Token: "merge-token"}}}}
	if err := credentials.Save(path, file); err != nil {
		t.Fatal(err)
	}
	env := func(key string) string {
		if key == "GITEA_ACCESS_TOKEN" {
			return "environment-token"
		}
		if key == "GITEA_HOST" {
			return "https://wrong"
		}
		return ""
	}
	for _, name := range []string{"ai", "merge"} {
		spec, err := ResolveGitea(t.Context(), GiteaOptions{Instance: name, Getenv: env})
		if err != nil || spec.Host != "https://site" || spec.Token != map[string]string{"ai": "review-token", "merge": "merge-token"}[name] {
			t.Fatal(spec, err)
		}
	}
	for _, opt := range []GiteaOptions{{Instance: "missing"}, {Instance: "ai", Host: "https://other"}} {
		if _, err := ResolveGitea(t.Context(), opt); err == nil {
			t.Fatal("无效实例选择被接受")
		}
	}
	spec, err := ResolveGitea(t.Context(), GiteaOptions{Instance: "ai", Host: "https://site", Token: "override"})
	if err != nil || spec.Token != "override" {
		t.Fatal(spec, err)
	}
	_ = os.WriteFile(path, []byte("bad"), 0600)
	if _, err := ResolveGitea(t.Context(), GiteaOptions{Instance: "ai"}); err == nil {
		t.Fatal("损坏实例库被忽略")
	}
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if _, err := ResolveGitea(t.Context(), GiteaOptions{Instance: "ai"}); err == nil {
		t.Fatal("无法定位用户凭据未报错")
	}
}
