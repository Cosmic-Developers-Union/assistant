package status

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGlobalMentionSearchUsesAccountAndReturnsIndependentRepos(t *testing.T) {
	for _, scenario := range []string{"pages", "search-error", "missing-repo", "bad-repo", "detail-error"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/version":
					fmt.Fprint(w, `{"version":"1.27.0"}`)
				case r.URL.Path == "/api/v1/repos/issues/search":
					if r.URL.Query().Get("mentioned_by") != "custom-bot" || r.URL.Query().Get("state") != "open" || r.URL.Query().Get("type") != "pulls" {
						t.Error(r.URL)
					}
					if scenario == "search-error" {
						w.WriteHeader(400)
						return
					}
					if scenario == "missing-repo" {
						fmt.Fprint(w, `[{"number":1,"pull_request":{}}]`)
						return
					}
					if scenario == "bad-repo" {
						fmt.Fprint(w, `[{"number":1,"pull_request":{},"repository":{"full_name":"../repo"}}]`)
						return
					}
					if r.URL.Query().Get("page") == "1" {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
						fmt.Fprint(w, `[{"number":1,"pull_request":{},"repository":{"full_name":"outside/one"}},{"number":20}]`)
					} else {
						fmt.Fprint(w, `[{"number":2,"pull_request":{},"repository":{"full_name":"other/two"}}]`)
					}
				case strings.HasSuffix(r.URL.Path, "/pulls/1"):
					if scenario == "detail-error" {
						w.WriteHeader(400)
						return
					}
					fmt.Fprint(w, `{"number":1,"state":"open","head":{"sha":"h1"},"base":{"ref":"main"}}`)
				case strings.HasSuffix(r.URL.Path, "/pulls/2"):
					fmt.Fprint(w, `{"number":2,"state":"open","head":{"sha":"h2"},"base":{"ref":"main"}}`)
				default:
					t.Error(r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			pulls, err := client.ListMentionedPullRequests(t.Context(), "custom-bot")
			if scenario == "pages" {
				if err != nil || len(pulls) != 2 || pulls[0].Repository.FullName() != "outside/one" || pulls[1].HeadSHA != "h2" {
					t.Fatal(pulls, err)
				}
			} else if err == nil {
				t.Fatal("搜索错误被忽略")
			}
			if scenario == "detail-error" && len(pulls) != 1 {
				t.Fatal("一个候选失败截断了后续页", pulls)
			}
		})
	}
}
