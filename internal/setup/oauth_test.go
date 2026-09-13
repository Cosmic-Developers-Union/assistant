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
		if request.FormValue("client_secret") != wantSecret {
			http.Error(writer, "bad client_secret", http.StatusUnauthorized)
			return
		}
		switch request.FormValue("grant_type") {
		case "authorization_code":
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
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"access_token":  "token-1",
				"refresh_token": "refresh-1",
			})
		case "refresh_token":
			if request.FormValue("client_id") != "client-1" || request.FormValue("refresh_token") != "refresh-1" {
				http.Error(writer, "bad refresh", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"access_token":  "token-2",
				"refresh_token": "refresh-2",
			})
		default:
			http.Error(writer, "unsupported grant", http.StatusBadRequest)
		}
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
	if result.RefreshToken != "refresh-1" {
		t.Errorf("RefreshToken = %q, want refresh-1", result.RefreshToken)
	}
}

func TestRefreshOAuthToken(t *testing.T) {
	server := fakeOAuthProvider(t, "")
	access, refresh, err := RefreshOAuthToken(
		context.Background(), server.URL, "client-1", "", "refresh-1", server.Client(),
	)
	if err != nil {
		t.Fatalf("RefreshOAuthToken() error = %v", err)
	}
	if access != "token-2" || refresh != "refresh-2" {
		t.Errorf("access = %q refresh = %q", access, refresh)
	}
	if _, _, err := RefreshOAuthToken(
		context.Background(), server.URL, "client-1", "", "wrong", server.Client(),
	); err == nil {
		t.Error("RefreshOAuthToken(wrong refresh) error = nil, want error")
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

func TestBuildAuthorizeURLScope(t *testing.T) {
	without := buildAuthorizeURL("https://gitea.example.com", "cid", "http://127.0.0.1:1", "st", "ch", "")
	if strings.Contains(without, "scope=") {
		t.Errorf("空 scope 不应带参数：%s", without)
	}
	with := buildAuthorizeURL("https://gitea.example.com", "cid", "http://127.0.0.1:1", "st", "ch", "all")
	if !strings.Contains(with, "scope=all") {
		t.Errorf("应带 scope 参数：%s", with)
	}
}

func TestOAuthErrorHintsOnScopeMismatch(t *testing.T) {
	err := oauthError("a grant exists with different scope", "https://gitea.example.com/")
	message := err.Error()
	for _, want := range []string{
		"different scope",
		"https://gitea.example.com/user/settings/applications",
		"--oauth-scope",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error 缺少 %q：%s", want, message)
		}
	}
	plain := oauthError("access_denied", "https://gitea.example.com").Error()
	if strings.Contains(plain, "--oauth-scope") {
		t.Errorf("无关错误不应带 scope 提示：%s", plain)
	}
}

func TestOAuthTokenErrorHintsOnConfidentialClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":"invalid_client","error_description":"invalid client secret"}`))
	}))
	defer server.Close()
	_, err := requestOAuthToken(context.Background(), server.Client(), server.URL, url.Values{"grant_type": {"authorization_code"}})
	if err == nil || !strings.Contains(err.Error(), "--oauth-client-secret") {
		t.Errorf("error = %v, want confidential 客户端提示", err)
	}
}
