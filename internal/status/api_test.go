package status

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientVerifyAuthentication(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantFatal   bool
		wantError   bool
		wantMessage string
	}{
		{
			name:       "accepted token",
			statusCode: http.StatusOK,
			body:       `{"login":"acme"}`,
		},
		{
			name:        "rejected token",
			statusCode:  http.StatusUnauthorized,
			body:        `{"message":"invalid username, password or token"}`,
			wantFatal:   true,
			wantMessage: "invalid username, password or token",
		},
		{
			name:       "forbidden token",
			statusCode: http.StatusForbidden,
			body:       `{"message":"token does not have the required scope"}`,
			wantFatal:  true,
		},
		{
			name:        "wrong host",
			statusCode:  http.StatusNotFound,
			body:        `404 page not found`,
			wantFatal:   true,
			wantMessage: "GITEA_HOST 未指向 Gitea API",
		},
		{
			name:       "server error is transient",
			statusCode: http.StatusInternalServerError,
			body:       `boom`,
			wantError:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			err = client.VerifyAuthentication(t.Context())
			if test.wantFatal {
				if !IsFatalError(err) {
					t.Fatalf("VerifyAuthentication() error = %v, want fatal error", err)
				}
				if test.wantMessage != "" && !strings.Contains(err.Error(), test.wantMessage) {
					t.Errorf("error = %q, want message containing %q", err.Error(), test.wantMessage)
				}
				return
			}
			if test.wantError {
				if err == nil {
					t.Fatal("VerifyAuthentication() error = nil, want transient error")
				}
				if IsFatalError(err) {
					t.Errorf("VerifyAuthentication() error = %v, want transient error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyAuthentication() error = %v", err)
			}
		})
	}
}

func TestClientVerifyAuthenticationSendsToken(t *testing.T) {
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/user" {
			http.NotFound(writer, request)
			return
		}
		gotAuthorization = request.Header.Get("Authorization")
		_, _ = writer.Write([]byte(`{"login":"acme"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.VerifyAuthentication(t.Context()); err != nil {
		t.Fatalf("VerifyAuthentication() error = %v", err)
	}
	if gotAuthorization != "token secret" {
		t.Errorf("Authorization = %q", gotAuthorization)
	}
}

func TestIsFatalError(t *testing.T) {
	wrapped := fmt.Errorf("run sync: %w", &FatalError{Reason: "认证失败"})
	if !IsFatalError(wrapped) {
		t.Errorf("IsFatalError(wrapped) = false")
	}
	if IsFatalError(errors.New("network unreachable")) {
		t.Errorf("IsFatalError(plain error) = true")
	}
}

func TestClientListRepositoriesFollowsPagination(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/user/repos" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "token secret" {
			http.Error(writer, "missing token", http.StatusUnauthorized)
			return
		}

		switch request.URL.Query().Get("page") {
		case "1":
			writer.Header().Set(
				"Link",
				fmt.Sprintf("<%s/api/v1/user/repos?page=2&limit=50>; rel=\"next\"", server.URL),
			)
			_, _ = writer.Write([]byte(`[{"name":"one","owner":{"login":"acme"}}]`))
		case "2":
			_, _ = writer.Write([]byte(`[{"name":"two","owner":{"login":"acme"}}]`))
		default:
			http.Error(writer, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	repositories, err := client.ListRepositories(t.Context())
	if err != nil {
		t.Fatalf("ListRepositories() error = %v", err)
	}
	if len(repositories) != 2 || repositories[0].Name != "one" || repositories[1].Name != "two" {
		t.Fatalf("repositories = %+v", repositories)
	}
}

func TestClientListTriageIssuesFiltersOpenIssues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/issues" {
			http.NotFound(writer, request)
			return
		}
		wantQuery := url.Values{
			"labels": {triageLabelName},
			"limit":  {"50"},
			"page":   {"1"},
			"state":  {"open"},
			"type":   {"issues"},
		}
		if request.URL.Query().Encode() != wantQuery.Encode() {
			http.Error(writer, "unexpected query: "+request.URL.RawQuery, http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`[{"number":9,"title":"Need details","html_url":"https://example/issues/9"}]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	issues, err := client.ListTriageIssues(t.Context(), Repository{Owner: "acme", Name: "video"})
	if err != nil {
		t.Fatalf("ListTriageIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Index != 9 || issues[0].Title != "Need details" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestClientListReviewPullRequestsFiltersByLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/issues" {
			http.NotFound(writer, request)
			return
		}
		wantQuery := url.Values{
			"labels": {reviewLabelName},
			"limit":  {"50"},
			"page":   {"1"},
			"state":  {"open"},
			"type":   {"pulls"},
		}
		if request.URL.Query().Encode() != wantQuery.Encode() {
			http.Error(writer, "unexpected query: "+request.URL.RawQuery, http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`[{"number":12,"title":"Fix rendering","html_url":"https://example/pulls/12"}]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	pullRequests, err := client.ListReviewPullRequests(t.Context(), Repository{Owner: "acme", Name: "video"})
	if err != nil {
		t.Fatalf("ListReviewPullRequests() error = %v", err)
	}
	if len(pullRequests) != 1 || pullRequests[0].Index != 12 || pullRequests[0].Title != "Fix rendering" {
		t.Fatalf("pull requests = %+v", pullRequests)
	}
}

func TestClientListOpenIssuesMapsLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/issues" {
			http.NotFound(writer, request)
			return
		}
		wantQuery := url.Values{
			"limit": {"50"},
			"page":  {"1"},
			"state": {"open"},
			"type":  {"issues"},
		}
		if request.URL.Query().Encode() != wantQuery.Encode() {
			http.Error(writer, "unexpected query: "+request.URL.RawQuery, http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(
			`[{"number":5,"title":"Bug","html_url":"https://example/issues/5",` +
				`"labels":[{"id":7,"name":"status/triage","exclusive":true}]}]`,
		))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	issues, err := client.ListOpenIssues(t.Context(), Repository{Owner: "acme", Name: "video"})
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Index != 5 {
		t.Fatalf("issues = %+v", issues)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != (Label{ID: 7, Name: triageLabelName, Exclusive: true}) {
		t.Fatalf("labels = %+v", issues[0].Labels)
	}
}

func TestClientCloseIssue(t *testing.T) {
	var gotMethod, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/issues/9" {
			http.NotFound(writer, request)
			return
		}
		gotMethod = request.Method
		body := make([]byte, request.ContentLength)
		_, _ = request.Body.Read(body)
		gotBody = string(body)
		_, _ = writer.Write([]byte(`{"number":9,"state":"closed"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.CloseIssue(t.Context(), Repository{Owner: "acme", Name: "video"}, 9); err != nil {
		t.Fatalf("CloseIssue() error = %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if !strings.Contains(gotBody, `"state":"closed"`) {
		t.Errorf("body = %q", gotBody)
	}
}

// automerge 合并请求保持 squash 方式，并携带 delete_branch_after_merge：
// feature 分支随合并删除，PR 页面不残留已合并分支。
func TestClientMergePullRequestDeletesBranch(t *testing.T) {
	var gotMethod, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		// SDK 合并前先取 PR 详情以填充 head_commit_id
		if request.URL.Path == "/api/v1/repos/acme/video/pulls/21" {
			_, _ = writer.Write([]byte(`{"number":21,"head":{"label":"features/x","ref":"features/x","sha":"abc123"}}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/pulls/21/merge" {
			http.NotFound(writer, request)
			return
		}
		gotMethod = request.Method
		body := make([]byte, request.ContentLength)
		_, _ = request.Body.Read(body)
		gotBody = string(body)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.MergePullRequest(t.Context(), Repository{Owner: "acme", Name: "video"}, 21); err != nil {
		t.Fatalf("MergePullRequest() error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotBody, `"Do":"squash"`) {
		t.Errorf("body = %q, want squash style", gotBody)
	}
	if !strings.Contains(gotBody, `"delete_branch_after_merge":true`) {
		t.Errorf("body = %q, want delete_branch_after_merge", gotBody)
	}
}

func TestClientGetCombinedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/commits/abc123/status" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"state":"failure","sha":"abc123","statuses":[` +
			`{"id":1,"status":"failure","context":"build / test","target_url":"https://ci/runs/1"},` +
			`{"id":2,"status":"success","context":"lint"}]}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	statuses, err := client.GetCombinedStatus(t.Context(), Repository{Owner: "acme", Name: "video"}, "abc123")
	if err != nil {
		t.Fatalf("GetCombinedStatus() error = %v", err)
	}
	want := []CheckStatus{
		{Context: "build / test", State: "failure", TargetURL: "https://ci/runs/1"},
		{Context: "lint", State: "success"},
	}
	if !slices.Equal(statuses, want) {
		t.Fatalf("statuses = %+v, want %+v", statuses, want)
	}
}

func TestClientListBranchProtectionsForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"user should be an owner or a collaborator with admin write of a repository"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.ListBranchProtections(t.Context(), Repository{Owner: "acme", Name: "video"})
	if !IsPermissionError(err) {
		t.Fatalf("ListBranchProtections() error = %v, want PermissionError", err)
	}
}

func TestClientListBranchProtections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
			return
		}
		if request.URL.Path != "/api/v1/repos/acme/video/branch_protections" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`[{"rule_name":"main","enable_status_check":true,` +
			`"status_check_contexts":["build / test"]}]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	protections, err := client.ListBranchProtections(t.Context(), Repository{Owner: "acme", Name: "video"})
	if err != nil {
		t.Fatalf("ListBranchProtections() error = %v", err)
	}
	want := []BranchProtection{{RuleName: "main", EnableStatusCheck: true, Contexts: []string{"build / test"}}}
	if !slices.EqualFunc(protections, want, func(a, b BranchProtection) bool {
		return a.RuleName == b.RuleName && a.EnableStatusCheck == b.EnableStatusCheck &&
			slices.Equal(a.Contexts, b.Contexts)
	}) {
		t.Fatalf("protections = %+v, want %+v", protections, want)
	}
}

// 配置了独立令牌后，只有分支保护读取带管理员令牌，其他接口仍用基础令牌。
func TestClientListBranchProtectionsUsesDedicatedToken(t *testing.T) {
	var protectionsAuth, reposAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/version":
			_, _ = writer.Write([]byte(`{"version":"1.26.0"}`))
		case "/api/v1/repos/acme/video/branch_protections":
			protectionsAuth = request.Header.Get("Authorization")
			_, _ = writer.Write([]byte(`[]`))
		case "/api/v1/user/repos":
			reposAuth = request.Header.Get("Authorization")
			_, _ = writer.Write([]byte(`[]`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "base-token")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.UseBranchProtectionToken("admin-token"); err != nil {
		t.Fatalf("UseBranchProtectionToken() error = %v", err)
	}
	if _, err := client.ListBranchProtections(t.Context(), Repository{Owner: "acme", Name: "video"}); err != nil {
		t.Fatalf("ListBranchProtections() error = %v", err)
	}
	if _, err := client.ListRepositories(t.Context()); err != nil {
		t.Fatalf("ListRepositories() error = %v", err)
	}
	if protectionsAuth != "token admin-token" {
		t.Errorf("branch_protections Authorization = %q, want %q", protectionsAuth, "token admin-token")
	}
	if reposAuth != "token base-token" {
		t.Errorf("user/repos Authorization = %q, want %q", reposAuth, "token base-token")
	}
}

// 版本探测失败不能再 panic：gitea SDK v1.2.0 探测失败后 serverVersion 留 nil，
// 第二次走版本门禁的调用会 nil 解引用（常驻 daemon 直接崩）。客户端改成自己探一次：
// 探到就钉死版本（SDK 不再自拉），探不到就忽略版本门禁，让真实 API 错误浮出来。
func TestClientVersionProbeDoesNotPanicOnRetry(t *testing.T) {
	cases := []struct {
		name         string
		versionBody  string
		versionCode  int
		wantVersions int32
	}{
		{name: "探测成功", versionBody: `{"version":"1.26.0"}`, versionCode: http.StatusOK, wantVersions: 1},
		{name: "探测失败", versionBody: "boom", versionCode: http.StatusInternalServerError, wantVersions: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var versionCalls int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/version" {
					atomic.AddInt32(&versionCalls, 1)
					if testCase.versionCode == http.StatusOK {
						_, _ = writer.Write([]byte(testCase.versionBody))
						return
					}
					http.Error(writer, testCase.versionBody, testCase.versionCode)
					return
				}
				if request.URL.Path == "/api/v1/repos/acme/video/issues" {
					_, _ = writer.Write([]byte(`[]`))
					return
				}
				http.NotFound(writer, request)
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			// 第一轮：探测不通时这一轮就是「检测失败，稍后重试」，错误由调度循环记下
			// （修复前 SDK 正是在这里吃掉错误、把版本留成 nil）；第二轮必须不再 panic
			_, _ = client.ListReviewPullRequests(t.Context(), Repository{Owner: "acme", Name: "video"})
			if _, err := client.ListReviewPullRequests(t.Context(), Repository{Owner: "acme", Name: "video"}); err != nil {
				t.Fatalf("第二轮调用 error = %v", err)
			}
			if got := atomic.LoadInt32(&versionCalls); got != testCase.wantVersions {
				t.Errorf("版本探测次数 = %d, want %d（构造期探一次，SDK 不再自己探测）", got, testCase.wantVersions)
			}
		})
	}
}
