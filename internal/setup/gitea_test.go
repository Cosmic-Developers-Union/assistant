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

// Gitea 协作者列表端点的权限是对象（admin/push/pull 布尔）而不是字符串：
// 归一化必须正确，否则 branch-protection 的推导直接失效。
// 部分 Gitea 版本的列表条目权限是字符串（permission: "admin"）而非对象。
func TestListCollaboratorsAcceptsStringPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]any{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"login": "merge", "permission": "admin"},
			{"login": "ai", "permission": "write"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	admin := &giteaAdmin{host: server.URL, http: server.Client(), token: "t", log: func(string, ...any) {}}

	collaborators, err := admin.ListCollaborators(context.Background(), "acme/repo")
	if err != nil {
		t.Fatalf("ListCollaborators() error = %v", err)
	}
	want := []Collaborator{
		{Name: "merge", Permission: "admin"},
		{Name: "ai", Permission: "write"},
	}
	if !slices.Equal(collaborators, want) {
		t.Errorf("collaborators = %+v, want %+v", collaborators, want)
	}
}

func TestListCollaboratorsNormalizesPermissions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]any{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"login": "merge", "permissions": map[string]any{"admin": true, "push": true, "pull": true}},
			{"login": "ai", "permissions": map[string]any{"admin": false, "push": true, "pull": true}},
			{"login": "dev", "permissions": map[string]any{"admin": false, "push": false, "pull": true}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	admin := &giteaAdmin{host: server.URL, http: server.Client(), token: "t", log: func(string, ...any) {}}

	collaborators, err := admin.ListCollaborators(context.Background(), "acme/repo")
	if err != nil {
		t.Fatalf("ListCollaborators() error = %v", err)
	}
	want := []Collaborator{
		{Name: "merge", Permission: "admin"},
		{Name: "ai", Permission: "write"},
		{Name: "dev", Permission: "read"},
	}
	if !slices.Equal(collaborators, want) {
		t.Errorf("collaborators = %+v, want %+v", collaborators, want)
	}
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
