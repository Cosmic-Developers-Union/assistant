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
	"time"
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
		// 成功（200）不重试：仍是一次；失败（500）由 retryTransport 退避重试
		// 到上限 3 次——抖动站点上版本探测也受保护，探测轮数不变（每轮至多 3 次尝试）
		{name: "探测成功", versionBody: `{"version":"1.26.0"}`, versionCode: http.StatusOK, wantVersions: 1},
		{name: "探测失败", versionBody: "boom", versionCode: http.StatusInternalServerError, wantVersions: 3},
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

func TestReviewerRespondedOnHead(t *testing.T) {
	reviews := []Review{
		{User: "ai", State: ReviewStateApproved, CommitID: "head-1"},
	}
	if !ReviewerRespondedOnHead(reviews, "ai", "head-1") {
		t.Error("当前 head 上的批准应视为已回应")
	}
	if ReviewerRespondedOnHead(reviews, "ai", "head-2") {
		t.Error("head 推进后旧批准不再计数（需要重新评审）")
	}
	dismissed := []Review{{User: "ai", State: ReviewStateApproved, CommitID: "head-1", Dismissed: true}}
	if ReviewerRespondedOnHead(dismissed, "ai", "head-1") {
		t.Error("被 dismiss 的批准不算回应")
	}
	comment := []Review{{User: "ai", State: ReviewStateComment, CommitID: ""}}
	if !ReviewerRespondedOnHead(comment, "ai", "head-9") {
		t.Error("无 CommitID 的历史回应保守视为已回应")
	}
	if ReviewerRespondedOnHead(reviews, "bob", "head-1") {
		t.Error("回应必须来自 reviewer 本人")
	}
}

// ArmAutoMerge 按 Gitea 1.27.3 实测状态码分类：201 排定、409+「already
// scheduled」已在排定、200 检查已绿直接合并；409 的其他消息（Wrong commit
// ID 等）与 405 常规拒绝是失败，405「Please try again later」短暂重试。
func TestClientArmAutoMerge(t *testing.T) {
	tests := []struct {
		name       string
		statuses   []int  // 依序返回的状态码（耗尽后重复最后一个）
		body       string // 4xx/5xx 响应体
		wantResult AutoMergeArmResult
		wantError  string
		wantCalls  int
	}{
		{
			name:       "scheduled",
			statuses:   []int{http.StatusCreated},
			wantResult: AutoMergeArmed,
			wantCalls:  1,
		},
		{
			name:       "already scheduled",
			statuses:   []int{http.StatusConflict},
			body:       `{"message":"pull request is already scheduled to auto merge when checks succeed [pull_id: 2]"}`,
			wantResult: AutoMergeAlreadyArmed,
			wantCalls:  1,
		},
		{
			name:       "merged directly",
			statuses:   []int{http.StatusOK},
			wantResult: AutoMergeMergedNow,
			wantCalls:  1,
		},
		{
			name:      "wrong head is a failure not already-armed",
			statuses:  []int{http.StatusConflict},
			body:      `{"message":"Wrong commit ID"}`,
			wantError: "Wrong commit ID",
			wantCalls: 1,
		},
		{
			name:       "mergeability pending retries then schedules",
			statuses:   []int{http.StatusMethodNotAllowed, http.StatusCreated},
			body:       `{"message":"Please try again later"}`,
			wantResult: AutoMergeArmed,
			wantCalls:  2,
		},
		{
			name:      "mergeability pending gives up after three tries",
			statuses:  []int{http.StatusMethodNotAllowed, http.StatusMethodNotAllowed, http.StatusMethodNotAllowed},
			body:      `{"message":"Please try again later"}`,
			wantError: "Please try again later",
			wantCalls: 3,
		},
		{
			name:      "other rejections do not retry",
			statuses:  []int{http.StatusMethodNotAllowed},
			body:      `{"message":"User not allowed to merge PR"}`,
			wantError: "User not allowed to merge PR",
			wantCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/version" {
					_, _ = writer.Write([]byte(`{"version":"1.27.3"}`))
					return
				}
				calls.Add(1)
				if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/pulls/21/merge") {
					t.Errorf("request = %s %s, want POST .../pulls/21/merge", request.Method, request.URL.Path)
				}
				index := min(int(calls.Load()), len(test.statuses)) - 1
				status := test.statuses[index]
				if status < 300 {
					writer.WriteHeader(status)
					return
				}
				writer.WriteHeader(status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			result, err := client.ArmAutoMerge(t.Context(), Repository{Owner: "acme", Name: "video"}, 21, "head")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("ArmAutoMerge() = %v, %v, want error containing %q", result, err, test.wantError)
				}
			} else if err != nil {
				t.Fatalf("ArmAutoMerge() error = %v", err)
			}
			if result != test.wantResult {
				t.Errorf("ArmAutoMerge() = %q, want %q", result, test.wantResult)
			}
			if got := int(calls.Load()); got != test.wantCalls {
				t.Errorf("calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

// DisarmAutoMerge 幂等：204 移除了排定，404 本无排定，其余为失败。
func TestClientDisarmAutoMerge(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantRemoved bool
		wantError   bool
	}{
		{name: "removed", statusCode: http.StatusNoContent, wantRemoved: true},
		{name: "nothing scheduled", statusCode: http.StatusNotFound},
		{name: "failure", statusCode: http.StatusForbidden, body: `{"message":"forbidden"}`, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/version" {
					_, _ = writer.Write([]byte(`{"version":"1.27.3"}`))
					return
				}
				if request.Method != http.MethodDelete {
					t.Errorf("request method = %s, want DELETE", request.Method)
				}
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			removed, err := client.DisarmAutoMerge(t.Context(), Repository{Owner: "acme", Name: "video"}, 21)
			if test.wantError {
				if err == nil {
					t.Fatalf("DisarmAutoMerge() error = nil, want failure")
				}
				return
			}
			if err != nil {
				t.Fatalf("DisarmAutoMerge() error = %v", err)
			}
			if removed != test.wantRemoved {
				t.Errorf("DisarmAutoMerge() removed = %v, want %v", removed, test.wantRemoved)
			}
		})
	}
}

// 评审请求时刻只认「assignee 是本 reviewer」的 review_request 事件：别的
// reviewer 的请求与其它类型的事件（评论、标签）都不算。
func TestClientListReviewRequestEventsFiltersByReviewer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/repos/acme/video/issues/199/timeline" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "token secret" {
			http.Error(writer, "missing token", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`[
			{"type":"comment","assignee":null,"created_at":"2026-09-25T23:25:19+08:00"},
			{"type":"review_request","assignee":{"login":"bob"},"created_at":"2026-09-25T23:21:38+08:00"},
			{"type":"review_request","assignee":{"login":"ai"},"created_at":"2026-09-25T23:05:27+08:00"},
			{"type":"review_request","assignee_team":{"name":"reviewers"},"created_at":"2026-09-25T23:30:00+08:00"},
			{"type":"review_request","assignee":{"login":"ai"},"created_at":"2026-09-25T23:34:08+08:00"},
			{"type":"label","assignee":null,"created_at":"2026-09-25T23:07:16+08:00"}
		]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	events, err := client.ListReviewRequestEvents(
		t.Context(), Repository{Owner: "acme", Name: "video"}, 199, "ai")
	if err != nil {
		t.Fatalf("ListReviewRequestEvents() error = %v", err)
	}
	want := []time.Time{
		time.Date(2026, 9, 25, 23, 5, 27, 0, time.FixedZone("", 8*3600)),
		time.Date(2026, 9, 25, 23, 34, 8, 0, time.FixedZone("", 8*3600)),
	}
	assertInstants(t, events, want)
}

// 时间戳缺失或非法的单条事件按「无法定位时刻」跳过，不让一条异常事件打断
// 整轮请求维护。
func TestClientListReviewRequestEventsSkipsMalformedTimestamps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`[
			{"type":"review_request","assignee":{"login":"ai"},"created_at":"not-a-time"},
			{"type":"review_request","assignee":{"login":"ai"}},
			{"type":"review_request","assignee":{"login":"ai"},"created_at":"2026-09-25T23:05:27Z"}
		]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	events, err := client.ListReviewRequestEvents(
		t.Context(), Repository{Owner: "acme", Name: "video"}, 199, "ai")
	if err != nil {
		t.Fatalf("ListReviewRequestEvents() error = %v", err)
	}
	want := []time.Time{time.Date(2026, 9, 25, 23, 5, 27, 0, time.UTC)}
	assertInstants(t, events, want)
}

// 满页即认为还有下一页（原始 HTTP 路径没有 SDK 的分页元数据，按 nextPage 的
// 兜底分支同口径推进）。
func TestClientListReviewRequestEventsFollowsPagination(t *testing.T) {
	pageSizeCount := pageSize
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("page") {
		case "1":
			events := make([]string, 0, pageSizeCount)
			for range pageSizeCount {
				events = append(events, `{"type":"label","assignee":null,"created_at":"2026-09-25T23:00:00Z"}`)
			}
			_, _ = writer.Write([]byte("[" + strings.Join(events, ",") + "]"))
		case "2":
			_, _ = writer.Write([]byte(
				`[{"type":"review_request","assignee":{"login":"ai"},"created_at":"2026-09-25T23:34:08Z"}]`))
		default:
			http.Error(writer, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	events, err := client.ListReviewRequestEvents(
		t.Context(), Repository{Owner: "acme", Name: "video"}, 199, "ai")
	if err != nil {
		t.Fatalf("ListReviewRequestEvents() error = %v", err)
	}
	want := []time.Time{time.Date(2026, 9, 25, 23, 34, 8, 0, time.UTC)}
	assertInstants(t, events, want)
}

// 非 2xx 时把 Gitea 的 message 带进错误：调用方据此放弃本轮撤回。
func TestClientListReviewRequestEventsReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"message":"boom"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.ListReviewRequestEvents(
		t.Context(), Repository{Owner: "acme", Name: "video"}, 199, "ai")
	if err == nil {
		t.Fatal("ListReviewRequestEvents() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want 含服务端 message", err)
	}
}

// assertInstants 按时间点（而非 time.Time 的时区表示）比较两个时刻序列：
// API 返回的时刻带 Gitea 的服务端时区，断言只关心瞬时值。
func assertInstants(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for index := range want {
		if !got[index].Equal(want[index]) {
			t.Errorf("events[%d] = %v, want %v", index, got[index], want[index])
		}
	}
}

// 传输层失败（连接被拒）与响应体不是 JSON 都要报错，且错误里带上目标编号，
// 便于运维定位是哪条 PR 的时间线读不到。
func TestClientListReviewRequestEventsReportsBadResponses(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close() // 关掉后端口无人监听
		client, err := NewClient(url, "secret")
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := client.ListReviewRequestEvents(
			t.Context(), Repository{Owner: "acme", Name: "video"}, 199, "ai"); err == nil {
			t.Fatal("ListReviewRequestEvents() error = nil, want failure")
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"not":"an array"}`))
		}))
		defer server.Close()
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		_, err = client.ListReviewRequestEvents(
			t.Context(), Repository{Owner: "acme", Name: "video"}, 199, "ai")
		if err == nil {
			t.Fatal("ListReviewRequestEvents() error = nil, want failure")
		}
		if !strings.Contains(err.Error(), "#199") {
			t.Errorf("error = %v, want 含 PR 编号", err)
		}
	})
}
