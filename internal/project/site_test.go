package project

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

type fixture struct {
	admin                bool
	existing             bool
	oldValid             bool
	wrongUser            bool
	emptyToken           bool
	defaultBranch        string
	protection           bool
	failMethod, failPath string
	requests             []string
	bodies               []map[string]any
}

func newFixture() *fixture { return &fixture{admin: true, defaultBranch: "main"} }
func (f *fixture) client(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		if r.Method != "GET" {
			var body map[string]any
			_ = json.UnmarshalRead(r.Body, &body)
			f.bodies = append(f.bodies, body)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == f.failMethod && r.URL.Path == f.failPath {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"message":"失败"}`)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/version":
			fmt.Fprint(w, `{"version":"1.27.0"}`)
		case r.URL.Path == "/api/v1/user":
			if r.Header.Get("Authorization") == "token old-token" {
				if !f.oldValid {
					w.WriteHeader(401)
					return
				}
				fmt.Fprint(w, `{"login":"ai"}`)
				return
			}
			if user, _, ok := r.BasicAuth(); ok {
				if f.wrongUser {
					user = "wrong"
				}
				fmt.Fprintf(w, `{"login":%q}`, user)
				return
			}
			fmt.Fprintf(w, `{"login":"admin","is_admin":%t}`, f.admin)
		case r.URL.Path == "/api/v1/users/ai":
			if !f.existing {
				w.WriteHeader(404)
				return
			}
			fmt.Fprint(w, `{"login":"ai"}`)
		case r.URL.Path == "/api/v1/admin/users":
			fmt.Fprint(w, `{"login":"ai"}`)
		case r.URL.Path == "/api/v1/users/ai/tokens":
			token := "new-token"
			if f.emptyToken {
				token = ""
			}
			fmt.Fprintf(w, `{"sha1":%q}`, token)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if r.Method == "GET" {
				fmt.Fprint(w, `[]`)
			} else {
				body := f.bodies[len(f.bodies)-1]
				fmt.Fprintf(w, `{"id":%d,"name":%q}`, len(f.bodies), body["name"])
			}
		case strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"id":1,"name":"label","exclusive":true}`)
		case strings.Contains(r.URL.Path, "/collaborators/"):
			w.WriteHeader(204)
		case strings.Contains(r.URL.Path, "/branch_protections"):
			if r.Method == "GET" {
				if !f.protection {
					w.WriteHeader(404)
					return
				}
				fmt.Fprint(w, `{"rule_name":"main","status_check_contexts":["test"]}`)
			} else {
				fmt.Fprint(w, `{"rule_name":"main"}`)
			}
		case strings.Contains(r.URL.Path, "/actions/secrets/"):
			w.WriteHeader(204)
		case r.URL.Path == "/api/v1/repos/team/repo":
			fmt.Fprintf(w, `{"name":"repo","default_branch":%q}`, f.defaultBranch)
		default:
			t.Errorf("未知请求 %s", key)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(credentials.Gitea{Name: "admin", URL: server.URL, Username: "admin", Token: "admin-token"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func TestProvisionCreatesAccountAndCredentialWithoutPersistingPassword(t *testing.T) {
	f := newFixture()
	client := f.client(t)
	entry, err := client.ProvisionAccount(t.Context(), "bots-ai", "ai", "ai@assistant.invalid", "", nil, false)
	if err != nil || entry.Token != "new-token" || entry.Username != "ai" {
		t.Fatal(entry, err)
	}
	created, tokened := false, false
	for _, req := range f.requests {
		created = created || req == "POST /api/v1/admin/users"
		tokened = tokened || req == "POST /api/v1/users/ai/tokens"
	}
	if !created || !tokened {
		t.Fatal(f.requests)
	}
	for _, body := range f.bodies {
		if password, ok := body["password"]; ok {
			if len(password.(string)) < 20 || body["must_change_password"] != false {
				t.Fatal(body)
			}
		}
	}
	data, _ := json.Marshal(entry)
	if strings.Contains(string(data), "password") {
		t.Fatal("密码进入凭据")
	}
}
func TestProvisionReusesValidCredentialsAndDryRunNeverWrites(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f := newFixture()
		f.existing = true
		f.oldValid = true
		client := f.client(t)
		old := credentials.Gitea{Name: "bots-ai", URL: client.Entry.URL, Username: "ai", Token: "old-token"}
		entry, err := client.ProvisionAccount(t.Context(), "bots-ai", "ai", "ai@assistant.invalid", "", &old, dry)
		if err != nil || entry != old {
			t.Fatal(entry, err)
		}
		for _, req := range f.requests {
			if !strings.HasPrefix(req, "GET ") {
				t.Fatal("重复接入写入平台", req)
			}
		}
	}
	f := newFixture()
	client := f.client(t)
	if _, err := client.ProvisionAccount(t.Context(), "bots-ai", "ai", "ai@assistant.invalid", "", nil, true); err != nil {
		t.Fatal(err)
	}
	for _, req := range f.requests {
		if !strings.HasPrefix(req, "GET ") {
			t.Fatal(req)
		}
	}
}
func TestProvisionDoesNotResetExistingPasswordsAndChecksIdentity(t *testing.T) {
	for _, scenario := range []string{"existing-no-password", "not-admin", "mismatch", "old-invalid", "user-failure", "create-failure", "token-failure", "identity-failure", "empty-token", "invalid-name"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture()
			client := f.client(t)
			var old *credentials.Gitea
			password, user := "", "ai"
			switch scenario {
			case "existing-no-password":
				f.existing = true
			case "not-admin":
				f.admin = false
			case "mismatch":
				old = &credentials.Gitea{URL: client.Entry.URL, Username: "other", Token: "old-token"}
			case "old-invalid":
				old = &credentials.Gitea{URL: client.Entry.URL, Username: "ai", Token: "old-token"}
			case "user-failure":
				f.failMethod = "GET"
				f.failPath = "/api/v1/users/ai"
			case "create-failure":
				f.failMethod = "POST"
				f.failPath = "/api/v1/admin/users"
			case "token-failure":
				f.failMethod = "POST"
				f.failPath = "/api/v1/users/ai/tokens"
			case "identity-failure":
				f.wrongUser = true
			case "empty-token":
				f.emptyToken = true
			case "invalid-name":
				user = "../ai"
			}
			if _, err := client.ProvisionAccount(t.Context(), "bots-ai", user, "ai@assistant.invalid", password, old, false); err == nil {
				t.Fatal("非法接入被接受")
			}
			for _, req := range f.requests {
				if strings.HasPrefix(req, "PATCH /api/v1/admin/users") {
					t.Fatal("重置了已有密码")
				}
			}
		})
	}
	f := newFixture()
	f.existing = true
	client := f.client(t)
	if _, err := client.ProvisionAccount(t.Context(), "bots-ai", "ai", "ai@assistant.invalid", "explicit-password", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := IssueToken(t.Context(), "invalid", "ai", "secret"); err == nil {
		t.Fatal("无效站点被接受")
	}
	if _, err := NewClient(credentials.Gitea{URL: "invalid"}); err == nil {
		t.Fatal("无效站点被接受")
	}
}

func TestTokenScopesKeepSiteAdminSeparateFromBots(t *testing.T) {
	for _, admin := range []bool{true, false} {
		scopes := TokenScopes(admin)
		found := false
		for _, scope := range scopes {
			if scope == "write:admin" {
				found = true
			}
			if scope == "all" {
				t.Fatal("工具请求了全权限")
			}
		}
		if found != admin {
			t.Fatal(scopes, admin)
		}
	}
}
