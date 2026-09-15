package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"assistant/internal/credentials"
)

// personalTokenServer 模拟 Gitea 的个人令牌端点（Basic Auth + X-Gitea-OTP）。
// 令牌值不可回读：列表只回末 8 位。
type personalTokenServer struct {
	t          *testing.T
	user       string
	password   string
	totp       string
	creates    int
	deletes    []string
	scopes     []string
	created    string
	failCreate bool
	existing   []map[string]any
}

func (s *personalTokenServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authUser, authPassword, ok := r.BasicAuth()
		if !ok || authUser != s.user || authPassword != s.password {
			s.t.Error("令牌端点必须用账号自己的 Basic Auth")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.totp != "" && r.Header.Get("X-Gitea-OTP") != s.totp {
			s.t.Error("TOTP 未通过 X-Gitea-OTP 传递")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		base := "/api/v1/users/" + s.user + "/tokens"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			if err := json.NewEncoder(w).Encode(s.existing); err != nil {
				s.t.Error(err)
			}
		case r.Method == http.MethodPost && r.URL.Path == base:
			s.creates++
			if s.failCreate {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"password secret must not be echoed"}`)
				return
			}
			var body struct {
				Name   string
				Scopes []string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				s.t.Error(err)
			}
			s.scopes = body.Scopes
			s.created = body.Name
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"sha1":"fresh-mcp-token"}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/"):
			s.deletes = append(s.deletes, strings.TrimPrefix(r.URL.Path, base+"/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestEnsureUserTokenRotatesSameName(t *testing.T) {
	state := &personalTokenServer{
		t: t, user: "alice", password: "s3cret", totp: "123456",
		existing: []map[string]any{
			{"id": 7, "name": "assistant-mcp-gitea.example.com-alice", "token_last_eight": "deadbeef"},
			{"id": 8, "name": "keep-me", "token_last_eight": "cafebabe"},
		},
	}
	server := httptest.NewServer(state.handler())
	defer server.Close()

	token, replaced, err := EnsureUserToken(context.Background(), server.URL, "alice", "s3cret", "123456",
		"assistant-mcp-gitea.example.com-alice", credentials.BotScopes())
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh-mcp-token" || replaced != 1 {
		t.Fatalf("token=%q replaced=%d", token, replaced)
	}
	if len(state.deletes) != 1 || state.deletes[0] != "7" {
		t.Fatalf("deletes = %v, want 只删同名旧令牌", state.deletes)
	}
	if state.creates != 1 || state.created != "assistant-mcp-gitea.example.com-alice" {
		t.Fatalf("creates=%d name=%q", state.creates, state.created)
	}
	want := []string{"read:repository", "write:repository", "read:issue", "write:issue", "read:user"}
	if len(state.scopes) != len(want) {
		t.Fatalf("scopes = %v", state.scopes)
	}
	for index, scope := range want {
		if state.scopes[index] != scope {
			t.Fatalf("scopes = %v, want %v", state.scopes, want)
		}
	}
}

// 创建失败时不回显服务端响应体（避免密码/提示语泄露），也不误报成功。
func TestEnsureUserTokenCreateFailure(t *testing.T) {
	state := &personalTokenServer{t: t, user: "alice", password: "s3cret", failCreate: true}
	server := httptest.NewServer(state.handler())
	defer server.Close()

	_, _, err := EnsureUserToken(context.Background(), server.URL, "alice", "s3cret", "", "assistant-mcp-x-alice", credentials.BotScopes())
	if err == nil {
		t.Fatal("创建失败必须报错")
	}
	if strings.Contains(err.Error(), "secret must not") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("错误信息泄露了服务端响应或密码：%v", err)
	}
	if !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("错误信息应给出可行动的替代方案：%v", err)
	}
}

func TestEnsureUserTokenRejectsBadInput(t *testing.T) {
	if _, _, err := EnsureUserToken(context.Background(), "not a url", "alice", "p", "", "n", credentials.BotScopes()); err == nil {
		t.Fatal("无效站点地址应报错")
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	if _, _, err := EnsureUserToken(context.Background(), server.URL, "alice", "", "", "n", credentials.BotScopes()); err == nil {
		t.Fatal("缺密码应报错（不发起请求）")
	}
}
