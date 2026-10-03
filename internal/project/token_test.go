package project

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

func TestIssueTokenSeparatesActorAndTarget(t *testing.T) {
	for _, scenario := range []string{"self", "other", "other-admin", "ordinary-denied", "actor-mismatch", "target-mismatch", "missing-target", "auth-error", "create-error", "empty-token", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			target := "ai"
			if scenario == "self" {
				target = "admin"
			}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				user, password, ok := r.BasicAuth()
				if !ok || user != "admin" || password != "actor-secret" || r.Header.Get("X-GITEA-OTP") != "123456" {
					t.Error("认证未保持为登录账号")
					w.WriteHeader(401)
					return
				}
				switch r.URL.Path {
				case "/api/v1/user":
					if scenario == "auth-error" {
						w.WriteHeader(401)
						fmt.Fprint(w, `{"message":"actor-secret actor-token"}`)
						return
					}
					name := "admin"
					if scenario == "actor-mismatch" {
						name = "other"
					}
					fmt.Fprintf(w, `{"login":%q,"is_admin":%t}`, name, scenario != "ordinary-denied")
				case "/api/v1/users/ai":
					if scenario == "missing-target" {
						w.WriteHeader(404)
						return
					}
					name := "ai"
					if scenario == "target-mismatch" {
						name = "wrong"
					}
					fmt.Fprintf(w, `{"login":%q,"is_admin":%t}`, name, scenario == "other-admin")
				case "/api/v1/users/" + target + "/tokens":
					writes++
					if r.Method != "POST" {
						t.Error("令牌请求不是 POST")
					}
					var body struct {
						Name   string   `json:"name"`
						Scopes []string `json:"scopes"`
					}
					if err := json.UnmarshalRead(r.Body, &body); err != nil {
						t.Error(err)
					}
					adminScope := slices.Contains(body.Scopes, "write:admin")
					if adminScope != (scenario == "self" || scenario == "other-admin") || !strings.HasPrefix(body.Name, "assistant-bot-") {
						t.Error("目标令牌范围或名称错误")
					}
					if scenario == "create-error" {
						w.WriteHeader(403)
						fmt.Fprint(w, `{"message":"actor-secret actor-token"}`)
						return
					}
					if scenario == "redirect" {
						http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
						return
					}
					token := "issued-token"
					if scenario == "empty-token" {
						token = ""
					}
					fmt.Fprintf(w, `{"sha1":%q}`, token)
				default:
					t.Error("未知请求", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			actor := credentials.Gitea{URL: server.URL, Username: "admin", Token: "actor-token"}
			token, err := IssueTokenForUser(t.Context(), actor, target, "actor-secret", "123456")
			if scenario == "self" || scenario == "other" || scenario == "other-admin" {
				if err != nil || token != "issued-token" || writes != 1 {
					t.Fatal(token, err, writes)
				}
			} else {
				if err == nil || token != "" {
					t.Fatal("失败被忽略", token, err)
				}
				if strings.Contains(err.Error(), "actor-secret") || strings.Contains(err.Error(), "actor-token") {
					t.Fatal("错误暴露凭据")
				}
				if strings.Contains(scenario, "mismatch") || scenario == "ordinary-denied" || scenario == "auth-error" || scenario == "missing-target" {
					if writes != 0 {
						t.Fatal("校验失败仍创建令牌")
					}
				}
			}
		})
	}
	for _, invalid := range []struct{ host, user, password string }{{"invalid", "ai", "secret"}, {"https://example.com", "../ai", "secret"}, {"https://example.com", "ai", ""}} {
		if _, err := IssueTokenForUser(t.Context(), credentials.Gitea{URL: invalid.host}, invalid.user, invalid.password, ""); err == nil {
			t.Fatal("接受无效输入")
		}
	}
}
