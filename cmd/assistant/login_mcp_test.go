package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"assistant/internal/instances"
)

func TestMCPPasswordLogin(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/users/developer/tokens":
					creates++
					user, pass, ok := r.BasicAuth()
					if r.Method != "POST" || !ok || user != "developer" || pass != " password " || r.Header.Get("X-Gitea-OTP") != "123456" {
						t.Error("password authentication or TOTP was not passed correctly")
					}
					var body struct {
						Name   string
						Scopes []string
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !strings.HasPrefix(body.Name, "assistant-mcp-") || !reflect.DeepEqual(body.Scopes,
						[]string{"write:repository", "write:issue", "write:organization", "write:package", "read:user"}) {
						t.Errorf("unexpected token creation payload: %+v", body)
					}
					if fail {
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(w, `{"message":"password secret must not be echoed"}`)
						return
					}
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"sha1":"new-mcp-token"}`)
				case "/api/v1/user":
					if !strings.Contains(r.Header.Get("Authorization"), "new-mcp-token") {
						t.Error("identity check did not use new token")
					}
					fmt.Fprint(w, `{"login":"developer"}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := instances.Save(path, &instances.File{Instances: []instances.Instance{
				{Host: server.URL, AdminToken: "admin-token", MCPToken: "old-mcp-token"},
				{Host: "https://other.example.com", MCPToken: "other-mcp-token"},
			}}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			tea := filepath.Join(dir, "tea.yml")
			if err := os.WriteFile(tea, []byte("logins:\n- url: "+server.URL+"\n  user: developer\n  token: tea-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEA_CONFIG", tea)
			var out bytes.Buffer
			cmd := newLoginCommand(&path)
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetIn(strings.NewReader(" password \n"))
			cmd.SetArgs([]string{server.URL, "--mcp", "--password-stdin", "--totp", "123456"})
			err := cmd.Execute()
			if (err != nil) != fail || creates != 1 {
				t.Fatalf("creates=%d, err=%v", creates, err)
			}
			after, _ := os.ReadFile(path)
			if fail {
				if !bytes.Equal(before, after) {
					t.Fatal("failed login changed config")
				}
				if strings.Contains(err.Error(), "secret must not") {
					t.Fatal("server response leaked into error")
				}
				return
			}
			file, err := instances.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if file.Instances[0].MCPToken != "new-mcp-token" || file.Instances[0].MCPUser != "developer" ||
				file.Instances[0].AdminToken != "admin-token" || file.Instances[1].MCPToken != "other-mcp-token" {
				t.Fatal("MCP login did not isolate target instance and credential")
			}
			for _, secret := range []string{"new-mcp-token", " password ", "123456", "tea-token"} {
				if strings.Contains(out.String(), secret) {
					t.Fatal("secret leaked in command output")
				}
			}
			if strings.Contains(string(after), "password") || strings.Contains(string(after), "tea-token") {
				t.Fatal("bootstrap credentials were saved")
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0o600 {
				t.Fatal("credential file must be private")
			}
		})
	}
}

func TestMCPTokenFileLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "file-token") {
			t.Error("wrong token")
		}
		fmt.Fprint(w, `{"login":"developer"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	path, tokenPath := filepath.Join(dir, "config.json"), filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newLoginCommand(&path)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{server.URL, "--mcp", "--token-file", tokenPath, "--user", "developer"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	file, err := instances.Load(path)
	if err != nil || file.Instances[0].MCPToken != "file-token" || file.Instances[0].MCPUser != "developer" {
		t.Fatalf("failed to import: %v", err)
	}
}

// 录入令牌时 --user 是身份断言：令牌实际属于别的账号必须拒绝并保持配置不变。
func TestMCPLoginRejectsIdentityMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"login":"someone-else"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, &instances.File{Instances: []instances.Instance{
		{Host: server.URL, MCPToken: "old-mcp-token", MCPUser: "alice"},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newLoginCommand(&path)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{server.URL, "--mcp", "--token", "other-token", "--user", "alice"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "someone-else") {
		t.Fatalf("身份不符应报错并指出实际账号，got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("身份不符时不应改动配置")
	}
}
