package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestConvergeTokenKeepsMatchingAndDeletesOthers(t *testing.T) {
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
	if names := store.names(); len(names) != 1 || names[0] != "assistant" {
		t.Errorf("tokens = %v, want only the kept one", names)
	}
}

func TestConvergeTokenCreatesAndDeletesAll(t *testing.T) {
	store := newTokenStore(
		tokenEntry(1, "assistant-setup-1", "old-token-deadbeef"),
		tokenEntry(2, "stale", "other-token-cafebabe"),
	)
	admin := testAdmin(t, store)

	token, created, err := admin.ConvergeToken(context.Background(), "ai", "password", ReviewerTokenName, "")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if !created || !strings.HasPrefix(token, "tok-"+ReviewerTokenName+"-") {
		t.Errorf("token = %q created = %v, want fresh reviewer token", token, created)
	}
	if names := store.names(); len(names) != 1 || names[0] != ReviewerTokenName {
		t.Errorf("tokens = %v, want only the fresh one", names)
	}
}

func TestEnsureRepoTokenKeepsOtherRepoTokens(t *testing.T) {
	store := newTokenStore(
		tokenEntry(1, RepoTokenName("acme/repo-a"), "token-a-11111111"),
		tokenEntry(2, RepoTokenName("acme/repo-b"), "token-b-22222222"),
		tokenEntry(3, "assistant", "legacy-token-33333333"),
	)
	admin := testAdmin(t, store)

	token, created, err := admin.EnsureRepoToken(
		context.Background(), "merge", "password", RepoTokenName("acme/repo-b"), "token-b-22222222",
	)
	if err != nil {
		t.Fatalf("EnsureRepoToken() error = %v", err)
	}
	if created || token != "token-b-22222222" {
		t.Errorf("token = %q created = %v, want kept repo token", token, created)
	}
	names := store.names()
	if len(names) != 2 {
		t.Fatalf("tokens = %v, want repo-a + repo-b kept, legacy removed", names)
	}
	if !contains(names, RepoTokenName("acme/repo-a")) || !contains(names, RepoTokenName("acme/repo-b")) {
		t.Errorf("tokens = %v, want both repo tokens", names)
	}
}

func TestEnsureRepoTokenReplacesSameName(t *testing.T) {
	repoName := RepoTokenName("acme/repo-a")
	store := newTokenStore(
		tokenEntry(1, repoName, "old-token-deadbeef"),
		tokenEntry(2, RepoTokenName("acme/repo-b"), "token-b-22222222"),
	)
	admin := testAdmin(t, store)

	token, created, err := admin.EnsureRepoToken(context.Background(), "merge", "password", repoName, "")
	if err != nil {
		t.Fatalf("EnsureRepoToken() error = %v", err)
	}
	if !created || !strings.HasPrefix(token, "tok-"+repoName+"-") {
		t.Errorf("token = %q created = %v, want fresh repo token", token, created)
	}
	names := store.names()
	if len(names) != 2 || !contains(names, RepoTokenName("acme/repo-b")) {
		t.Errorf("tokens = %v, want new repo-a + untouched repo-b", names)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
