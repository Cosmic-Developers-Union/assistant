package setup

import (
	"context"
	"encoding/json"
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
	creates int
}

func newTokenStore(tokens ...map[string]any) *tokenStore {
	return &tokenStore{tokens: tokens, nextID: int64(len(tokens) + 1)}
}

func (s *tokenStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/ai/tokens", func(writer http.ResponseWriter, request *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, _, ok := request.BasicAuth(); !ok {
			http.Error(writer, `{"message":"auth required"}`, http.StatusUnauthorized)
			return
		}
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(writer).Encode(s.tokens)
		case http.MethodPost:
			s.creates++
			_ = json.NewEncoder(writer).Encode(map[string]string{"sha1": "fresh-token-cafebabe"})
		default:
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/users/ai/tokens/", func(writer http.ResponseWriter, request *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if request.Method != http.MethodDelete {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(request.URL.Path, "/api/v1/users/ai/tokens/"), 10, 64)
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
	})
	return mux
}

func TestConvergeTokenKeepsMatchingAndDeletesOthers(t *testing.T) {
	store := newTokenStore(
		map[string]any{"id": int64(1), "name": "assistant", "token_last_eight": "deadbeef"},
		map[string]any{"id": int64(2), "name": "stale", "token_last_eight": "cafebabe"},
	)
	server := httptest.NewServer(store.handler())
	defer server.Close()
	admin := &giteaAdmin{host: server.URL, http: server.Client(), log: func(string, ...any) {}}

	token, created, err := admin.ConvergeToken(context.Background(), "ai", "password", "old-token-deadbeef")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if token != "old-token-deadbeef" || created {
		t.Errorf("token = %q created = %v, want kept token", token, created)
	}
	if store.creates != 0 {
		t.Errorf("creates = %d, want 0", store.creates)
	}
	if len(store.tokens) != 1 || store.tokens[0]["id"].(int64) != 1 {
		t.Errorf("tokens = %+v, want only the kept one", store.tokens)
	}
}

func TestConvergeTokenCreatesAndDeletesAll(t *testing.T) {
	store := newTokenStore(
		map[string]any{"id": int64(1), "name": "assistant-setup-1", "token_last_eight": "deadbeef"},
		map[string]any{"id": int64(2), "name": "assistant", "token_last_eight": "cafebabe"},
	)
	server := httptest.NewServer(store.handler())
	defer server.Close()
	admin := &giteaAdmin{host: server.URL, http: server.Client(), log: func(string, ...any) {}}

	token, created, err := admin.ConvergeToken(context.Background(), "ai", "password", "")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if token != "fresh-token-cafebabe" || !created {
		t.Errorf("token = %q created = %v, want fresh token", token, created)
	}
	if store.creates != 1 {
		t.Errorf("creates = %d, want 1", store.creates)
	}
	if len(store.tokens) != 0 {
		t.Errorf("tokens = %+v, want all deleted before recreate", store.tokens)
	}
}
