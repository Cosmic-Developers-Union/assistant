package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	gitea "gitea.dev/sdk"
)

// tokenStore 模拟 Gitea 的账号令牌端点（列出/创建/删除，Basic Auth）。
type tokenStore struct {
	mu      sync.Mutex
	tokens  []map[string]any
	nextID  int64
	nextSeq int
	creates int
}

func newTokenStore(tokens ...map[string]any) *tokenStore {
	return &tokenStore{tokens: tokens, nextID: int64(len(tokens) + 1)}
}

func tokenEntry(id int64, name, value string) map[string]any {
	lastEight := value
	if len(lastEight) > 8 {
		lastEight = lastEight[len(lastEight)-8:]
	}
	return map[string]any{"id": id, "name": name, "token_last_eight": lastEight}
}

func (s *tokenStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/", func(writer http.ResponseWriter, request *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, _, ok := request.BasicAuth(); !ok {
			http.Error(writer, `{"message":"auth required"}`, http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/users/"), "/")
		if len(parts) < 2 || parts[1] != "tokens" {
			http.NotFound(writer, request)
			return
		}
		switch {
		case len(parts) == 2:
			switch request.Method {
			case http.MethodGet:
				_ = json.NewEncoder(writer).Encode(s.tokens)
			case http.MethodPost:
				var body struct {
					Name string `json:"name"`
				}
				_ = json.NewDecoder(request.Body).Decode(&body)
				s.creates++
				s.nextSeq++
				value := fmt.Sprintf("tok-%s-%d", body.Name, s.nextSeq)
				s.tokens = append(s.tokens, tokenEntry(s.nextID, body.Name, value))
				s.nextID++
				_ = json.NewEncoder(writer).Encode(map[string]string{"sha1": value})
			default:
				http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			}
		case len(parts) == 3:
			if request.Method != http.MethodDelete {
				http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			id, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				http.Error(writer, "bad id", http.StatusBadRequest)
				return
			}
			kept := s.tokens[:0]
			found := false
			for _, token := range s.tokens {
				if token["id"].(int64) == id {
					found = true
					continue
				}
				kept = append(kept, token)
			}
			s.tokens = kept
			if !found {
				http.Error(writer, "not found", http.StatusNotFound)
			}
		default:
			http.NotFound(writer, request)
		}
	})
	return mux
}

func (s *tokenStore) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tokens))
	for _, token := range s.tokens {
		names = append(names, token["name"].(string))
	}
	return names
}

func testAdmin(t *testing.T, store *tokenStore) *giteaAdmin {
	t.Helper()
	server := httptest.NewServer(store.handler())
	t.Cleanup(server.Close)
	return &giteaAdmin{host: server.URL, http: server.Client(), log: func(string, ...any) {}}
}

// ListCollaborators 走官方 SDK：列表拿用户名，CollaboratorPermission 服务端
// 计算有效权限（owner 归一化为 admin）。
func TestListCollaboratorsNormalizesPermissions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"login": "merge"}, {"login": "ai"}, {"login": "dev"},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators/merge/permission", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"permission": "admin"})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators/ai/permission", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"permission": "write"})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators/dev/permission", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"permission": "owner"})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	admin := newSDKAdmin(t, server)

	collaborators, err := admin.ListCollaborators(context.Background(), "acme/repo")
	if err != nil {
		t.Fatalf("ListCollaborators() error = %v", err)
	}
	want := []Collaborator{
		{Name: "merge", Permission: "admin"},
		{Name: "ai", Permission: "write"},
		{Name: "dev", Permission: "admin"},
	}
	if !slices.Equal(collaborators, want) {
		t.Errorf("collaborators = %+v, want %+v", collaborators, want)
	}
}

// 仓库枚举只走 /repos/search（/admin/repos 已在 Gitea 1.27 移除）。
func TestListAllReposUsesSearch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/search", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("private") != "true" {
			http.Error(writer, "want private=true", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"ok":   true,
			"data": []map[string]any{{"full_name": "xwh/AI-video"}, {"full_name": "xwh/other"}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	admin := &giteaAdmin{host: server.URL, http: server.Client(), token: "t",
		log: func(string, ...any) {}}

	repos, err := admin.ListAllRepos(context.Background())
	if err != nil {
		t.Fatalf("ListAllRepos() error = %v", err)
	}
	want := []string{"xwh/AI-video", "xwh/other"}
	if !slices.Equal(repos, want) {
		t.Errorf("repos = %v, want %v", repos, want)
	}
}

// newSDKAdmin 构造带官方 SDK 客户端的 giteaAdmin（ListCollaborators 需要）。
func newSDKAdmin(t *testing.T, server *httptest.Server) *giteaAdmin {
	t.Helper()
	sdk, err := gitea.NewClient(server.URL,
		gitea.SetToken("t"),
		gitea.SetHTTPClient(server.Client()),
		gitea.SetUserAgent("assistant-test/1"),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return &giteaAdmin{host: server.URL, http: server.Client(), token: "t", sdk: sdk, log: func(string, ...any) {}}
}

func TestConvergeTokenKeepsMatchingAndLeavesOthers(t *testing.T) {
	store := newTokenStore(
		tokenEntry(1, "assistant", "old-token-deadbeef"),
		tokenEntry(2, "stale", "other-token-cafebabe"),
	)
	admin := testAdmin(t, store)

	token, created, err := admin.ConvergeToken(context.Background(), "ai", "password", ReviewerTokenName, "old-token-deadbeef")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if token != "old-token-deadbeef" || created {
		t.Errorf("token = %q created = %v, want kept token", token, created)
	}
	if store.creates != 0 {
		t.Errorf("creates = %d, want 0", store.creates)
	}
	names := store.names()
	if len(names) != 2 {
		t.Fatalf("tokens = %v, want kept one plus untouched others", names)
	}
	if !slices.Contains(names, "assistant") || !slices.Contains(names, "stale") {
		t.Errorf("tokens = %v, want both assistant and stale kept", names)
	}
}

func TestConvergeTokenReplacesOnlyOwnedToken(t *testing.T) {
	store := newTokenStore(
		tokenEntry(1, "assistant", "old-token-deadbeef"),
		tokenEntry(2, "human-made", "other-token-cafebabe"),
	)
	admin := testAdmin(t, store)

	token, created, err := admin.ConvergeToken(context.Background(), "ai", "password", ReviewerTokenName, "")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if !created || !strings.HasPrefix(token, "tok-"+ReviewerTokenName+"-") {
		t.Errorf("token = %q created = %v, want fresh reviewer token", token, created)
	}
	names := store.names()
	if len(names) != 2 {
		t.Fatalf("tokens = %v, want fresh one plus untouched human-made", names)
	}
	if !slices.Contains(names, ReviewerTokenName) || !slices.Contains(names, "human-made") {
		t.Errorf("tokens = %v, want fresh assistant token and human-made kept", names)
	}
}
