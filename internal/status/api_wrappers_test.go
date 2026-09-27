package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
)

// newWrapperClient 构造指向假 Gitea 的 Client，并返回该假服务的 mux 以便登记路由。
// 这类测试覆盖的是「薄包装」层：路径、方法、请求体、错误包装与分页推进——
// 目标仓库与服务端依赖这些形状，所以虽然每个函数很短，仍必须逐一对齐。
func newWrapperClient(t *testing.T) (*Client, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client, mux
}

// 写操作的包装：路径、方法与请求体都按 Gitea API 约定发出，失败时错误带语境。
func TestClientWriteWrappers(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	for _, test := range []struct {
		name     string
		path     string
		method   string
		wantBody []string // 请求体应包含的片段
		// respondAs 覆盖默认响应体（个别端点 SDK 期望特定结构，如标签数组）
		respondAs string
		call      func(*Client) error
	}{
		{
			name:     "CreateIssueComment",
			path:     "/api/v1/repos/acme/video/issues/7/comments",
			method:   http.MethodPost,
			wantBody: []string{"body", "请补充说明"},
			call: func(c *Client) error {
				return c.CreateIssueComment(t.Context(), repository, 7, "请补充说明")
			},
		},
		{
			name:      "AddLabel",
			path:      "/api/v1/repos/acme/video/issues/7/labels",
			method:    http.MethodPost,
			wantBody:  []string{"labels", "42"},
			respondAs: "[]", // SDK 的 AddLabel 期望标签数组
			call: func(c *Client) error {
				return c.AddLabel(t.Context(), repository, 7, 42)
			},
		},
		{
			name:   "RemoveLabel",
			path:   "/api/v1/repos/acme/video/issues/7/labels/42",
			method: http.MethodDelete,
			call: func(c *Client) error {
				return c.RemoveLabel(t.Context(), repository, 7, 42)
			},
		},
		{
			name:     "CreatePullReview",
			path:     "/api/v1/repos/acme/video/pulls/7/reviews",
			method:   http.MethodPost,
			wantBody: []string{"APPROVED", "lgtm", "deadbeef"},
			call: func(c *Client) error {
				return c.CreatePullReview(t.Context(), repository, 7, ReviewInput{
					State: ReviewStateApproved, Body: "lgtm", CommitID: "deadbeef",
				})
			},
		},
		{
			name:     "CreateReviewRequests",
			path:     "/api/v1/repos/acme/video/pulls/7/requested_reviewers",
			method:   http.MethodPost,
			wantBody: []string{"reviewers", "ai"},
			call: func(c *Client) error {
				return c.CreateReviewRequests(t.Context(), repository, 7, []string{"ai"})
			},
		},
		{
			name:   "DeleteReviewRequests",
			path:   "/api/v1/repos/acme/video/pulls/7/requested_reviewers",
			method: http.MethodDelete,
			call: func(c *Client) error {
				return c.DeleteReviewRequests(t.Context(), repository, 7, []string{"ai"})
			},
		},
		{
			name:     "CreateLabel",
			path:     "/api/v1/repos/acme/video/labels",
			method:   http.MethodPost,
			wantBody: []string{"status/review", "0ff000"},
			call: func(c *Client) error {
				_, err := c.CreateLabel(t.Context(), repository, LabelDefinition{Name: "status/review", Color: "0ff000"})
				return err
			},
		},
		{
			name:   "DeleteLabel",
			path:   "/api/v1/repos/acme/video/labels/42",
			method: http.MethodDelete,
			call: func(c *Client) error {
				return c.DeleteLabel(t.Context(), repository, 42)
			},
		},
		{
			name:   "SetLabelExclusive",
			path:   "/api/v1/repos/acme/video/labels/42",
			method: http.MethodPatch,
			call: func(c *Client) error {
				return c.SetLabelExclusive(t.Context(), repository, 42)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, mux := newWrapperClient(t)
			var gotMethod string
			var gotBody []byte
			mux.HandleFunc(test.path, func(writer http.ResponseWriter, request *http.Request) {
				gotMethod = request.Method
				gotBody, _ = readAllLimited(request)
				body := test.respondAs
				if body == "" {
					body = `{}`
				}
				_, _ = writer.Write([]byte(body))
			})

			if err := test.call(client); err != nil {
				t.Fatalf("%s error = %v", test.name, err)
			}
			if gotMethod != test.method {
				t.Errorf("method = %s, want %s", gotMethod, test.method)
			}
			for _, fragment := range test.wantBody {
				if !strings.Contains(string(gotBody), fragment) {
					t.Errorf("body = %s, 缺少 %q", gotBody, fragment)
				}
			}
		})
	}
}

// 读操作的包装：分页推进与条目映射。
func TestClientReadWrappers(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}

	t.Run("ListOpenPullRequests", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/pulls", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode([]map[string]any{{
				"number": 7, "title": "add thing", "state": "open", "mergeable": true,
				"head":                map[string]any{"sha": "head-7", "ref": "feature"},
				"base":                map[string]any{"sha": "base-1", "ref": "main"},
				"requested_reviewers": []map[string]any{{"login": "ai"}, {"login": ""}},
			}})
		})

		pulls, err := client.ListOpenPullRequests(t.Context(), repository)
		if err != nil {
			t.Fatalf("ListOpenPullRequests() error = %v", err)
		}
		if len(pulls) != 1 {
			t.Fatalf("pulls = %+v", pulls)
		}
		got := pulls[0]
		if got.Index != 7 || got.HeadSHA != "head-7" || got.BaseRef != "main" || !got.Open {
			t.Errorf("pull = %+v", got)
		}
		// 空用户名的 reviewer 要被过滤掉（避免把空字符串当成一个 reviewer）
		if len(got.RequestedReviewers) != 1 || got.RequestedReviewers[0] != "ai" {
			t.Errorf("RequestedReviewers = %v, want [ai]", got.RequestedReviewers)
		}
	})

	t.Run("GetPullRequest", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/pulls/7", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"number": 7, "title": "t", "state": "open"})
		})

		pull, err := client.GetPullRequest(t.Context(), repository, 7)
		if err != nil {
			t.Fatalf("GetPullRequest() error = %v", err)
		}
		if pull.Index != 7 {
			t.Errorf("pull = %+v", pull)
		}
	})

	t.Run("ListPullReviews", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/pulls/7/reviews", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode([]map[string]any{{
				"id": 3, "state": "APPROVED", "commit_id": "head-7", "official": true,
				"submitted_at": "2026-09-25T23:34:57Z",
				"user":         map[string]any{"login": "ai"},
			}})
		})

		reviews, err := client.ListPullReviews(t.Context(), repository, 7)
		if err != nil {
			t.Fatalf("ListPullReviews() error = %v", err)
		}
		if len(reviews) != 1 || reviews[0].User != "ai" || reviews[0].State != ReviewStateApproved {
			t.Fatalf("reviews = %+v", reviews)
		}
		if reviews[0].CommitID != "head-7" || !reviews[0].Official {
			t.Errorf("review = %+v（commit_id 与 official 必须映射）", reviews[0])
		}
		if reviews[0].Submitted.IsZero() {
			t.Error("Submitted 未映射（完成判定依赖它在会话起点之后）")
		}
	})

	t.Run("ListIssueCommentsSince", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/issues/7/comments", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode([]map[string]any{{
				"id": 1, "body": "看下", "created_at": "2026-09-25T23:25:19Z",
				"user": map[string]any{"login": "Ge"},
			}})
		})

		comments, err := client.ListIssueCommentsSince(t.Context(), repository, 7, time.Time{})
		if err != nil {
			t.Fatalf("ListIssueCommentsSince() error = %v", err)
		}
		if len(comments) != 1 || comments[0].User != "Ge" || comments[0].Body != "看下" {
			t.Fatalf("comments = %+v", comments)
		}
		if comments[0].Created.IsZero() {
			t.Error("Created 未映射")
		}
	})

	t.Run("GetIssueLabels", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/issues/7/labels", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode([]map[string]any{{"id": 5, "name": "status/review", "exclusive": true}})
		})

		labels, err := client.GetIssueLabels(t.Context(), repository, 7)
		if err != nil {
			t.Fatalf("GetIssueLabels() error = %v", err)
		}
		if len(labels) != 1 || labels[0].Name != "status/review" || !labels[0].Exclusive {
			t.Errorf("labels = %+v", labels)
		}
	})

	t.Run("ListCollaboratorLogins", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/collaborators", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode([]map[string]any{{"login": "ai"}, {"login": "merge"}})
		})

		logins, err := client.ListCollaboratorLogins(t.Context(), repository)
		if err != nil {
			t.Fatalf("ListCollaboratorLogins() error = %v", err)
		}
		if len(logins) != 2 || logins[0] != "ai" || logins[1] != "merge" {
			t.Errorf("logins = %v", logins)
		}
	})

	t.Run("ListIssuesMentioning", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/issues", func(writer http.ResponseWriter, request *http.Request) {
			if got := request.URL.Query().Get("mentioned_by"); got != "ai" {
				t.Errorf("mentioned_by = %q, want ai（服务端过滤是 mention 通道的关键）", got)
			}
			_ = json.NewEncoder(writer).Encode([]map[string]any{{"number": 9, "title": "看看", "state": "open"}})
		})

		issues, err := client.ListIssuesMentioning(t.Context(), repository, "ai", "issues")
		if err != nil {
			t.Fatalf("ListIssuesMentioning() error = %v", err)
		}
		if len(issues) != 1 || issues[0].Index != 9 {
			t.Errorf("issues = %+v", issues)
		}
	})

	t.Run("AuthenticatedUser", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"login": "ai", "is_admin": true})
		})

		login, err := client.AuthenticatedUser(t.Context())
		if err != nil {
			t.Fatalf("AuthenticatedUser() error = %v", err)
		}
		if login != "ai" {
			t.Errorf("login = %q, want ai", login)
		}
	})

	t.Run("AuthenticatedIdentity", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"login": "Ge", "is_admin": true})
		})

		login, admin, err := client.AuthenticatedIdentity(t.Context())
		if err != nil {
			t.Fatalf("AuthenticatedIdentity() error = %v", err)
		}
		if login != "Ge" || !admin {
			t.Errorf("identity = %q admin=%t, want Ge/true", login, admin)
		}
	})

	t.Run("GetRepository", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video", func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"default_branch": "main", "private": true, "empty": false,
			})
		})

		details, err := client.GetRepository(t.Context(), "acme", "video")
		if err != nil {
			t.Fatalf("GetRepository() error = %v", err)
		}
		if details.DefaultBranch != "main" || !details.Private || details.Empty {
			t.Errorf("details = %+v", details)
		}
	})
}

// 认证身份的两个函数在服务端返回缺用户名时必须报错（不能返回空字符串——
// 那会让调用方把「未知身份」当成一个真实账号）。
func TestAuthenticatedIdentityRejectsMissingUsername(t *testing.T) {
	client, mux := newWrapperClient(t)
	mux.HandleFunc("/api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{}`))
	})

	if _, err := client.AuthenticatedUser(t.Context()); err == nil {
		t.Error("AuthenticatedUser() error = nil, want 缺用户名的错误")
	}
	if _, _, err := client.AuthenticatedIdentity(t.Context()); err == nil {
		t.Error("AuthenticatedIdentity() error = nil, want 缺用户名的错误")
	}
}

// 服务端错误必须带语境（调用方据此定位是哪个操作、哪个 PR 失败）。
func TestClientWrappersReportErrors(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	for _, test := range []struct {
		name    string
		path    string
		want    string
		wantErr bool
		call    func(*Client) error
	}{
		{
			name: "CreateIssueComment", path: "/api/v1/repos/acme/video/issues/7/comments",
			call: func(c *Client) error { return c.CreateIssueComment(t.Context(), repository, 7, "x") },
		},
		{
			name: "CreatePullReview", path: "/api/v1/repos/acme/video/pulls/7/reviews",
			call: func(c *Client) error {
				return c.CreatePullReview(t.Context(), repository, 7, ReviewInput{State: ReviewStateApproved})
			},
		},
		{
			name: "AddLabel", path: "/api/v1/repos/acme/video/issues/7/labels",
			call: func(c *Client) error { return c.AddLabel(t.Context(), repository, 7, 1) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, mux := newWrapperClient(t)
			mux.HandleFunc(test.path, func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = writer.Write([]byte(`{"message":"boom"}`))
			})
			if err := test.call(client); err == nil {
				t.Errorf("%s error = nil, want 失败", test.name)
			}
		})
	}
}

// 空评论内容在本地就被拒绝：不浪费一次往返，也不在服务端留下空评论。
func TestCreateIssueCommentRejectsBlankBody(t *testing.T) {
	client, mux := newWrapperClient(t)
	called := false
	mux.HandleFunc("/api/v1/repos/acme/video/issues/7/comments", func(http.ResponseWriter, *http.Request) {
		called = true
	})

	for _, body := range []string{"", "   ", "\n\t"} {
		if err := client.CreateIssueComment(t.Context(), Repository{Owner: "acme", Name: "video"}, 7, body); err == nil {
			t.Errorf("CreateIssueComment(%q) error = nil, want 拒绝空内容", body)
		}
	}
	if called {
		t.Error("空内容不应发起请求")
	}
}

// 空 reviewer 列表是 no-op：不发起请求（重复请求在 Gitea 侧本就是 no-op）。
func TestReviewRequestWrappersSkipEmptyList(t *testing.T) {
	client, mux := newWrapperClient(t)
	var calls int32
	mux.HandleFunc("/api/v1/repos/acme/video/pulls/7/requested_reviewers", func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&calls, 1)
	})

	repository := Repository{Owner: "acme", Name: "video"}
	if err := client.CreateReviewRequests(t.Context(), repository, 7, nil); err != nil {
		t.Errorf("CreateReviewRequests(nil) error = %v", err)
	}
	if err := client.DeleteReviewRequests(t.Context(), repository, 7, nil); err != nil {
		t.Errorf("DeleteReviewRequests(nil) error = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("空列表发起了 %d 次请求, want 0", got)
	}
}

// UseStateReviewerToken("") 保持现状：状态驳回继续用基础令牌提交。
func TestUseStateReviewerTokenEmptyKeepsCurrent(t *testing.T) {
	client, _ := newWrapperClient(t)
	if err := client.UseStateReviewerToken(""); err != nil {
		t.Errorf("UseStateReviewerToken(\"\") error = %v, want nil（保持现状）", err)
	}
	if err := client.UseBranchProtectionToken(""); err != nil {
		t.Errorf("UseBranchProtectionToken(\"\") error = %v", err)
	}
}

// EnableRequestLog 之后每次请求都回调（方法、URL、状态码）；令牌不落日志。
func TestEnableRequestLogReportsRequests(t *testing.T) {
	client, mux := newWrapperClient(t)
	mux.HandleFunc("/api/v1/repos/acme/video", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"default_branch": "main"})
	})

	type logged struct {
		method, url string
		status      int
	}
	entries := make(chan logged, 4)
	client.EnableRequestLog(func(method, url string, status int, _ time.Duration) {
		entries <- logged{method, url, status}
	})

	if _, err := client.GetRepository(t.Context(), "acme", "video"); err != nil {
		t.Fatalf("GetRepository() error = %v", err)
	}

	select {
	case got := <-entries:
		if got.method != http.MethodGet {
			t.Errorf("method = %q, want GET", got.method)
		}
		if got.status != http.StatusOK {
			t.Errorf("status = %d, want 200", got.status)
		}
		if strings.Contains(got.url, "secret") {
			t.Errorf("URL 泄漏了令牌：%q", got.url)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("请求日志未被回调")
	}
}

// CheckHealth 探测 /api/v1/version：可达时返回版本；非 2xx 与连接失败都报错。
func TestCheckHealth(t *testing.T) {
	t.Run("返回版本号", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"version":"1.27.3"}`))
		})
		version, err := client.CheckHealth(t.Context())
		if err != nil {
			t.Fatalf("CheckHealth() error = %v", err)
		}
		if version != "1.27.3" {
			t.Errorf("version = %q, want 1.27.3", version)
		}
	})

	t.Run("非 JSON 体退回原文本", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte("1.2.3-plain"))
		})
		version, err := client.CheckHealth(t.Context())
		if err != nil {
			t.Fatalf("CheckHealth() error = %v", err)
		}
		if version != "1.2.3-plain" {
			t.Errorf("version = %q", version)
		}
	})

	t.Run("非 2xx 报错", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
		})
		if _, err := client.CheckHealth(t.Context()); err == nil {
			t.Error("CheckHealth() error = nil, want 失败")
		}
	})

	t.Run("地址不是 Gitea", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNotFound)
		}))
		server.Close()
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := client.CheckHealth(t.Context()); err == nil {
			t.Error("CheckHealth() error = nil, want 连接失败")
		}
	})
}

// pullRequestFromSDK 是纯映射函数：nil reviewer、空 head/base 都要安全。
func TestPullRequestFromSDKHandlesMissingFields(t *testing.T) {
	pull := pullRequestFromSDK(&gitea.PullRequest{Index: 5, Title: "t"})
	if pull.Index != 5 || pull.HeadSHA != "" || pull.BaseRef != "" {
		t.Errorf("pull = %+v", pull)
	}
	if len(pull.RequestedReviewers) != 0 || pull.RequestedReviewersTeams {
		t.Errorf("缺省时不应有 reviewer：%+v", pull)
	}
}

// 团队评审请求映射为布尔标记（assistant 无法按成员吸收，只能整体视为意图）。
func TestPullRequestFromSDKMapsTeams(t *testing.T) {
	pull := pullRequestFromSDK(&gitea.PullRequest{
		Index:                   5,
		RequestedReviewersTeams: []*gitea.Team{{Name: "reviewers"}},
	})
	if !pull.RequestedReviewersTeams {
		t.Error("RequestedReviewersTeams = false, want true")
	}
}

// readAllLimited 读取请求体（测试用，限长避免异常流量）。
func readAllLimited(request *http.Request) ([]byte, error) {
	defer request.Body.Close()
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	for {
		read, err := request.Body.Read(chunk)
		buffer = append(buffer, chunk[:read]...)
		if err != nil || len(buffer) > 1<<20 {
			return buffer, nil
		}
	}
}

var _ = context.Background

// ListPullRequestsRequestingReview 是调度队列的取数口径（「哪些 PR 待评审」），
// 必须与标签口径的 hasUnansweredReviewRequest 给出同一答案——两者曾经分裂，
// 导致复审请求被撤回却仍留在队列里的 PR 无人处理。这里直接对服务端行为
// 断言完整矩阵：请求在不在、回应落在哪个 head、是否团队请求。
func TestClientListPullRequestsRequestingReview(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}

	for _, test := range []struct {
		name        string
		pull        map[string]any
		reviews     []map[string]any
		wantPending bool
	}{
		{
			name: "被请求且未回应：待评审",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			wantPending: true,
		},
		{
			name: "已在当前 head 回应：出清",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			reviews: []map[string]any{
				{"id": 1, "state": "APPROVED", "commit_id": "head-1", "user": map[string]any{"login": "ai"}},
			},
		},
		{
			name: "回应落在旧 head：仍需评审（推送使旧回应失效）",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "new-head"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			reviews: []map[string]any{
				{"id": 1, "state": "APPROVED", "commit_id": "old-head", "user": map[string]any{"login": "ai"}},
			},
			wantPending: true,
		},
		{
			name: "回应被 dismiss：视为未回应",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			reviews: []map[string]any{
				{"id": 1, "state": "APPROVED", "commit_id": "head-1", "dismissed": true,
					"user": map[string]any{"login": "ai"}},
			},
			wantPending: true,
		},
		{
			name: "回应缺 commit_id：保守算已回应（不重复拉起）",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			reviews: []map[string]any{
				{"id": 1, "state": "COMMENT", "user": map[string]any{"login": "ai"}},
			},
		},
		{
			name: "只有别人的回应：待评审",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			reviews: []map[string]any{
				{"id": 1, "state": "APPROVED", "commit_id": "head-1", "user": map[string]any{"login": "bob"}},
			},
			wantPending: true,
		},
		{
			name: "没被请求：不在队列",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "bob"}},
			},
		},
		{
			name: "团队请求：恒视为待评审（无法按成员吸收）",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers_teams": []map[string]any{{"name": "reviewers"}},
			},
			wantPending: true,
		},
		{
			name: "评审请求只针对人员 reviewer：REQUEST_REVIEW 记录不算回应",
			pull: map[string]any{
				"number": 1, "state": "open", "head": map[string]any{"sha": "head-1"},
				"requested_reviewers": []map[string]any{{"login": "ai"}},
			},
			reviews: []map[string]any{
				{"id": 1, "state": "REQUEST_REVIEW", "user": map[string]any{"login": "ai"}},
			},
			wantPending: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, mux := newWrapperClient(t)
			mux.HandleFunc("/api/v1/repos/acme/video/pulls", func(writer http.ResponseWriter, _ *http.Request) {
				if test.pull == nil {
					_, _ = writer.Write([]byte(`[]`))
					return
				}
				_ = json.NewEncoder(writer).Encode([]map[string]any{test.pull})
			})
			mux.HandleFunc("/api/v1/repos/acme/video/pulls/1/reviews", func(writer http.ResponseWriter, _ *http.Request) {
				reviews := test.reviews
				if reviews == nil {
					reviews = []map[string]any{}
				}
				_ = json.NewEncoder(writer).Encode(reviews)
			})

			pending, err := client.ListPullRequestsRequestingReview(t.Context(), repository, "ai")
			if err != nil {
				t.Fatalf("ListPullRequestsRequestingReview() error = %v", err)
			}
			if got := len(pending) > 0; got != test.wantPending {
				t.Errorf("待评审 = %t, want %t（pending=%+v）", got, test.wantPending, pending)
			}
		})
	}
}

// 队列取数失败时带语境（定位到具体 PR），而不是静默返回空队列——空队列会被
// 上层当成「没有待办」，那是最危险的一种静默失败。
func TestClientListPullRequestsRequestingReviewReportsReviewError(t *testing.T) {
	client, mux := newWrapperClient(t)
	mux.HandleFunc("/api/v1/repos/acme/video/pulls", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode([]map[string]any{{
			"number": 7, "state": "open", "head": map[string]any{"sha": "head-7"},
			"requested_reviewers": []map[string]any{{"login": "ai"}},
		}})
	})
	mux.HandleFunc("/api/v1/repos/acme/video/pulls/7/reviews", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"message":"boom"}`))
	})

	_, err := client.ListPullRequestsRequestingReview(t.Context(), repositoryOf("acme", "video"), "ai")
	if err == nil {
		t.Fatal("error = nil, want 失败")
	}
	if !strings.Contains(err.Error(), "#7") {
		t.Errorf("error = %v, want 含 PR 编号", err)
	}
}

func repositoryOf(owner, name string) Repository {
	return Repository{Owner: owner, Name: name}
}

// DeleteLabel 与 RemoveLabel 的错误要带语境：标签维护失败必须能定位到哪个标签。
func TestLabelRemovalErrors(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}

	t.Run("DeleteLabel", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/labels/42", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
		})
		err := client.DeleteLabel(t.Context(), repository, 42)
		if err == nil {
			t.Fatal("DeleteLabel() error = nil, want 失败")
		}
		if !strings.Contains(err.Error(), "42") {
			t.Errorf("error = %v, want 含标签 ID", err)
		}
	})

	t.Run("RemoveLabel", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/issues/7/labels/42", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
		})
		if err := client.RemoveLabel(t.Context(), repository, 7, 42); err == nil {
			t.Error("RemoveLabel() error = nil, want 失败")
		}
	})
}

// MergePullRequest 必须检查 SDK 的 success 标志：Gitea 对 405 等拒绝只回
// bool=false 而不回 error，漏检会把「门禁拒绝」当成「合并成功」。
func TestMergePullRequestChecksSuccessFlag(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	client, mux := newWrapperClient(t)
	mux.HandleFunc("/api/v1/repos/acme/video/pulls/7/merge", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = writer.Write([]byte(`{"message":"Not allowed"}`))
	})

	if err := client.MergePullRequest(t.Context(), repository, 7); err == nil {
		t.Error("MergePullRequest() error = nil, want 把服务端拒绝报成失败")
	}
}

// CloseIssue 与 SetLabelExclusive 的错误路径。
func TestCloseAndExclusiveErrors(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}

	t.Run("CloseIssue", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/issues/7", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
		})
		if err := client.CloseIssue(t.Context(), repository, 7); err == nil {
			t.Error("CloseIssue() error = nil, want 失败")
		}
	})

	t.Run("SetLabelExclusive", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/labels/42", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
		})
		if err := client.SetLabelExclusive(t.Context(), repository, 42); err == nil {
			t.Error("SetLabelExclusive() error = nil, want 失败")
		}
	})

	t.Run("CreateLabel", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/labels", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"boom"}`))
		})
		if _, err := client.CreateLabel(t.Context(), repository, LabelDefinition{Name: "x", Color: "fff"}); err == nil {
			t.Error("CreateLabel() error = nil, want 失败")
		}
	})

	t.Run("GetPullRequest", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video/pulls/7", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"message":"not found"}`))
		})
		if _, err := client.GetPullRequest(t.Context(), repository, 7); err == nil {
			t.Error("GetPullRequest() error = nil, want 失败")
		}
	})

	t.Run("GetRepository", func(t *testing.T) {
		client, mux := newWrapperClient(t)
		mux.HandleFunc("/api/v1/repos/acme/video", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"message":"denied"}`))
		})
		if _, err := client.GetRepository(t.Context(), "acme", "video"); err == nil {
			t.Error("GetRepository() error = nil, want 失败")
		}
	})
}

// UseStateReviewerToken 与 UseBranchProtectionToken 配置独立令牌：
// 空 token 保持现状；非空 token 会为后续状态评审单独建 SDK 客户端。
func TestUseSeparateTokens(t *testing.T) {
	client, mux := newWrapperClient(t)
	mux.HandleFunc("/api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"version":"1.27.3"}`))
	})
	mux.HandleFunc("/api/v1/repos/acme/video/pulls/7/reviews", func(writer http.ResponseWriter, request *http.Request) {
		// 状态评审以独立令牌身份提交，授权头必须是那一份
		if got := request.Header.Get("Authorization"); got != "token state-token" {
			t.Errorf("Authorization = %q, want 状态评审令牌", got)
		}
		_, _ = writer.Write([]byte(`{}`))
	})

	if err := client.UseStateReviewerToken("state-token"); err != nil {
		t.Fatalf("UseStateReviewerToken() error = %v", err)
	}
	repository := Repository{Owner: "acme", Name: "video"}
	if err := client.CreatePullReview(t.Context(), repository, 7, ReviewInput{
		State: ReviewStateApproved, Body: "会签", CommitID: "head-7",
	}); err != nil {
		t.Fatalf("CreatePullReview() error = %v", err)
	}

	if err := client.UseBranchProtectionToken("bp-token"); err != nil {
		t.Fatalf("UseBranchProtectionToken() error = %v", err)
	}
}

// keepOnlyPrefixedLabel 的契约：目标标签缺失时报错（不能静默跳过——那会让
// 状态机分叉）；已带目标标签时不重复添加；同前缀的其它标签被移除。
func TestKeepOnlyPrefixedLabel(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}

	t.Run("目标标签不在仓库标签体系时报错", func(t *testing.T) {
		api := newFakeAPI(repository, completeLabels())
		manager := NewManager(api)
		err := manager.keepOnlyPrefixedLabel(t.Context(), repository,
			mergeablePullRequest(7, nil), map[string]Label{}, statusLabelPrefix, reviewLabelName)
		if err == nil {
			t.Error("error = nil, want 标签缺失错误")
		}
	})

	t.Run("已带目标标签时不重复添加", func(t *testing.T) {
		labels := completeLabels()
		reviewLabel := labelByName(t, labels, reviewLabelName)
		api := newFakeAPI(repository, labels)
		manager := NewManager(api)

		if err := manager.keepOnlyPrefixedLabel(t.Context(), repository,
			mergeablePullRequest(7, []Label{reviewLabel}),
			map[string]Label{reviewLabelName: reviewLabel}, statusLabelPrefix, reviewLabelName); err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(api.addedLabels) != 0 {
			t.Errorf("added = %+v, want 无（标签已在）", api.addedLabels)
		}
	})
}

// branchBehind / formatBranchState 的边界：缺 base 或 merge-base 时「未知」
// （不能猜成落后——那是会阻断合并的判定）。
func TestBranchBehindBoundaries(t *testing.T) {
	for _, test := range []struct {
		name       string
		pull       PullRequest
		wantBehind bool
		wantKnown  bool
	}{
		{
			name: "缺 base sha 时未知",
			pull: PullRequest{MergeBase: "mb"},
		},
		{
			name: "缺 merge-base 时未知",
			pull: PullRequest{BaseSHA: "base"},
		},
		{
			name:      "两者相同：未落后（已知且不落后）",
			pull:      PullRequest{BaseSHA: "base", MergeBase: "base"},
			wantKnown: true,
		},
		{
			name:       "不同：落后",
			pull:       PullRequest{BaseSHA: "base", MergeBase: "mb"},
			wantBehind: true,
			wantKnown:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			behind, known := branchBehind(test.pull)
			if behind != test.wantBehind || known != test.wantKnown {
				t.Errorf("branchBehind() = (%t, %t), want (%t, %t)",
					behind, known, test.wantBehind, test.wantKnown)
			}
			wantText := "unknown"
			if test.wantKnown {
				wantText = fmt.Sprint(test.wantBehind)
			}
			if got := formatBranchState(behind, known); got != wantText {
				t.Errorf("formatBranchState() = %q, want %q", got, wantText)
			}
		})
	}
}

// awaitingLabelForStatus 的映射是确定性的：review 归 reviewer、approved 归
// 合并人、其余归作者（「轮到谁行动」不能有歧义）。
func TestAwaitingLabelForStatus(t *testing.T) {
	for _, test := range []struct {
		status string
		want   string
	}{
		{status: reviewLabelName, want: awaitingReviewerLabelName},
		{status: approvedLabelName, want: awaitingMergeLabelName},
		{status: changesRequestedLabelName, want: awaitingAuthorLabelName},
		{status: inProgressLabelName, want: awaitingAuthorLabelName},
		{status: "未知状态", want: awaitingAuthorLabelName},
	} {
		if got := awaitingLabelForStatus(test.status); got != test.want {
			t.Errorf("awaitingLabelForStatus(%q) = %q, want %q", test.status, got, test.want)
		}
	}
}
