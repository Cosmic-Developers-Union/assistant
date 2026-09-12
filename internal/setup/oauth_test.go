package setup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeOAuthProvider 是最小的 OAuth2 provider 桩：authorize 直接带 code 回调，
// token 端点校验 PKCE，user 端点返回管理员身份。
func fakeOAuthProvider(t *testing.T, wantSecret string) *httptest.Server {
	t.Helper()
	var (
		mu        sync.Mutex
		challenge string
	)
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/login/oauth/authorize", func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		mu.Lock()
		challenge = query.Get("code_challenge")
		mu.Unlock()
		redirect, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			http.Error(writer, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		values := redirect.Query()
		values.Set("code", "code-1")
		values.Set("state", query.Get("state"))
		redirect.RawQuery = values.Encode()
		http.Redirect(writer, request, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/login/oauth/access_token", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		mu.Lock()
		wantChallenge := challenge
		mu.Unlock()
		digest := sha256.Sum256([]byte(request.FormValue("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(digest[:]) != wantChallenge {
			http.Error(writer, "bad code_verifier", http.StatusBadRequest)
			return
		}
		if request.FormValue("client_id") != "client-1" || request.FormValue("code") != "code-1" {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		if request.FormValue("client_secret") != wantSecret {
			http.Error(writer, "bad client_secret", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "token-1"})
	})
	mux.HandleFunc("/api/v1/user", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token-1" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"login": "admin", "is_admin": true})
	})
	return server
}

func TestOAuthLoginPublicClient(t *testing.T) {
	server := fakeOAuthProvider(t, "")
	result, err := OAuthLogin(context.Background(), OAuthOptions{
		Host:       server.URL,
		ClientID:   "client-1",
		HTTPClient: server.Client(),
		OpenBrowser: func(target string) error {
			response, err := server.Client().Get(target)
			if err != nil {
				return err
			}
			response.Body.Close()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("OAuthLogin() error = %v", err)
	}
	if result.Token != "token-1" || result.Login != "admin" || !result.IsAdmin {
		t.Errorf("result = %+v", result)
	}
}

func TestOAuthLoginSendsClientSecret(t *testing.T) {
	server := fakeOAuthProvider(t, "secret-1")
	if _, err := OAuthLogin(context.Background(), OAuthOptions{
		Host:         server.URL,
		ClientID:     "client-1",
		ClientSecret: "secret-1",
		HTTPClient:   server.Client(),
		OpenBrowser: func(target string) error {
			response, err := server.Client().Get(target)
			if err != nil {
				return err
			}
			response.Body.Close()
			return nil
		},
	}); err != nil {
		t.Fatalf("OAuthLogin() error = %v", err)
	}
}

func TestOAuthLoginRejectsStateMismatch(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/login/oauth/authorize", func(writer http.ResponseWriter, request *http.Request) {
		redirect, _ := url.Parse(request.URL.Query().Get("redirect_uri"))
		values := redirect.Query()
		values.Set("code", "code-1")
		values.Set("state", "wrong-state")
		redirect.RawQuery = values.Encode()
		http.Redirect(writer, request, redirect.String(), http.StatusFound)
	})

	_, err := OAuthLogin(context.Background(), OAuthOptions{
		Host:       server.URL,
		ClientID:   "client-1",
		HTTPClient: server.Client(),
		OpenBrowser: func(target string) error {
			response, err := server.Client().Get(target)
			if err != nil {
				return err
			}
			response.Body.Close()
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Errorf("OAuthLogin() error = %v, want state mismatch", err)
	}
}
