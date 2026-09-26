package status

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// auditFixture 是按统一策略配置好的服务端响应；可逐项篡改用于漂移测试。
type auditFixture struct {
	protection   map[string]any
	labels       []map[string]any
	collaborator map[string]string
	secrets      []map[string]any
}

func defaultAuditFixture() auditFixture {
	labels := make([]map[string]any, 0)
	for _, definition := range LabelDefinitions() {
		labels = append(labels, map[string]any{
			"id":        len(labels) + 1,
			"name":      definition.Name,
			"exclusive": definition.Exclusive,
		})
	}
	return auditFixture{
		protection: map[string]any{
			"rule_name":                         "main",
			"required_approvals":                2,
			"enable_merge_whitelist":            true,
			"merge_whitelist_usernames":         []string{"merge"},
			"block_on_rejected_reviews":         true,
			"block_on_official_review_requests": true,
			"block_admin_merge_override":        true,
			"dismiss_stale_approvals":           true,
			"block_on_outdated_branch":          true,
		},
		labels: labels,
		collaborator: map[string]string{
			"ai":    "write",
			"merge": "admin",
		},
		secrets: []map[string]any{{"name": "MERGE_TOKEN"}},
	}
}

func (f auditFixture) server(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(writer http.ResponseWriter, value any) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(value)
	}
	mux.HandleFunc("/api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]string{"version": "1.27.0"})
	})
	mux.HandleFunc("/api/v1/repos/acme/video/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, []any{f.protection})
	})
	mux.HandleFunc("/api/v1/repos/acme/video/labels", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, f.labels)
	})
	mux.HandleFunc("/api/v1/repos/acme/video/collaborators/", func(writer http.ResponseWriter, request *http.Request) {
		user := strings.TrimSuffix(strings.TrimPrefix(
			request.URL.Path, "/api/v1/repos/acme/video/collaborators/"), "/permission")
		permission, ok := f.collaborator[user]
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			writeJSON(writer, map[string]string{"message": "not found"})
			return
		}
		writeJSON(writer, map[string]string{"permission": permission})
	})
	mux.HandleFunc("/api/v1/repos/acme/video/actions/secrets", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, f.secrets)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAuditRepositoryAllGreen(t *testing.T) {
	client := defaultAuditFixture().server(t)
	findings, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{
		Reviewer: "ai", Merger: "merge", RequiredApprovals: 2,
	})
	if err != nil {
		t.Fatalf("AuditRepository() error = %v", err)
	}
	for _, finding := range findings {
		if !finding.OK() {
			t.Errorf("应全部正常: %+v", finding)
		}
	}
}

func TestAuditRepositoryDetectsDrift(t *testing.T) {
	fixture := defaultAuditFixture()
	fixture.protection["required_approvals"] = 1
	fixture.protection["merge_whitelist_usernames"] = []string{"everyone"}
	fixture.protection["block_on_official_review_requests"] = false
	fixture.protection["dismiss_stale_approvals"] = false
	fixture.labels = fixture.labels[:len(fixture.labels)-1]                                   // 少一个标签
	fixture.labels = append(fixture.labels, map[string]any{"id": 999, "name": "stale/topic"}) // 多一个非规范标签
	fixture.collaborator["ai"] = "read"
	fixture.secrets = []map[string]any{}
	client := fixture.server(t)

	findings, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{
		Reviewer: "ai", Merger: "merge", RequiredApprovals: 2,
	})
	if err != nil {
		t.Fatalf("AuditRepository() error = %v", err)
	}
	want := map[string]string{
		"branch protection approvals":         AuditStatusOutdated,
		"branch protection merge whitelist":   AuditStatusOutdated,
		"branch protection official requests": AuditStatusOutdated,
		"branch protection dismiss stale":     AuditStatusOutdated,
		"collaborator ai":                     AuditStatusOutdated,
		"label stale/topic":                   AuditStatusOutdated,
		"actions secret MERGE_TOKEN":          AuditStatusMissing,
	}
	for path, wantStatus := range want {
		found := false
		for _, finding := range findings {
			if finding.Path == path {
				found = true
				if finding.Status != wantStatus {
					t.Errorf("%s 状态 = %s, want %s", path, finding.Status, wantStatus)
				}
			}
		}
		if !found {
			t.Errorf("缺少 %s 的检查结果: %+v", path, findings)
		}
	}
}

func TestAuditRepositorySkipsWithoutPermission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/version" {
			_, _ = writer.Write([]byte(`{"version":"1.27.0"}`))
			return
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "reviewer-token")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{
		Reviewer: "ai", Merger: "merge", RequiredApprovals: 2,
	})
	if err != nil {
		t.Fatalf("AuditRepository() error = %v", err)
	}
	// 权限不足只让对应项 SKIPPED，其余检查照常执行（不整体报错、不整体跳过）
	wantPaths := []string{
		"branch protection main",
		"labels",
		"collaborator ai",
		"collaborator merge",
		"actions secret MERGE_TOKEN",
	}
	if len(findings) != len(wantPaths) {
		t.Fatalf("findings = %+v, want %d 项", findings, len(wantPaths))
	}
	for index, finding := range findings {
		if finding.Path != wantPaths[index] || finding.Status != AuditStatusSkipped || !finding.OK() {
			t.Errorf("findings[%d] = %+v, want %s/SKIPPED", index, finding, wantPaths[index])
		}
	}
}

// AuditRepository 的失败分支：每一项检查独立执行，权限不足标 SKIPPED，其它
// 错误（网络/5xx）必须上抛——不能把故障当成「配置没问题」放过。
func TestAuditRepositoryErrorBranches(t *testing.T) {
	t.Run("非权限错误上抛", func(t *testing.T) {
		fixture := defaultAuditFixture()
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/repos/acme/video/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte(`{"message":"upstream down"}`))
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{}); err == nil {
			t.Error("error = nil, want 5xx 上抛")
		}
		_ = fixture
	})

	t.Run("标签读取失败上抛", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/repos/acme/video/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusForbidden)
		})
		mux.HandleFunc("/api/v1/repos/acme/video/labels", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{}); err == nil {
			t.Error("error = nil, want 标签读取失败上抛")
		}
	})

	t.Run("协作者与 secret 的错误分支", func(t *testing.T) {
		fixture := defaultAuditFixture()
		mux := http.NewServeMux()
		writeJSON := func(writer http.ResponseWriter, value any) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(value)
		}
		mux.HandleFunc("/api/v1/repos/acme/video/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, []any{fixture.protection})
		})
		mux.HandleFunc("/api/v1/repos/acme/video/labels", func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, fixture.labels)
		})
		// ai 权限落入 5xx（非权限错误）⇒ 上抛
		mux.HandleFunc("/api/v1/repos/acme/video/collaborators/", func(writer http.ResponseWriter, request *http.Request) {
			if strings.Contains(request.URL.Path, "/ai/") {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeJSON(writer, map[string]string{"permission": "admin"})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{}); err == nil {
			t.Error("error = nil, want 协作者读取失败上抛")
		}
	})

	t.Run("权限不足时协作者与 secret 标 SKIPPED", func(t *testing.T) {
		fixture := defaultAuditFixture()
		mux := http.NewServeMux()
		writeJSON := func(writer http.ResponseWriter, value any) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(value)
		}
		mux.HandleFunc("/api/v1/repos/acme/video/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, []any{fixture.protection})
		})
		mux.HandleFunc("/api/v1/repos/acme/video/labels", func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, fixture.labels)
		})
		mux.HandleFunc("/api/v1/repos/acme/video/collaborators/", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusForbidden)
		})
		mux.HandleFunc("/api/v1/repos/acme/video/actions/secrets", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusForbidden)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		findings, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"}, AuditOptions{})
		if err != nil {
			t.Fatalf("AuditRepository() error = %v, want nil（权限不足只标 SKIPPED）", err)
		}
		var skipped int
		for _, finding := range findings {
			if finding.Status == AuditStatusSkipped {
				skipped++
			}
		}
		if skipped < 3 {
			t.Errorf("SKIPPED 项 = %d, want ≥3（branch protection/协作者/secret）：%+v", skipped, findings)
		}
	})

	t.Run("SkipSecrets 直接返回", func(t *testing.T) {
		client := defaultAuditFixture().server(t)
		findings, err := client.AuditRepository(t.Context(), Repository{Owner: "acme", Name: "video"},
			AuditOptions{Reviewer: "ai", Merger: "merge", RequiredApprovals: 2, SkipSecrets: true})
		if err != nil {
			t.Fatalf("AuditRepository() error = %v", err)
		}
		found := false
		for _, finding := range findings {
			if finding.Path == "actions secret MERGE_TOKEN" && finding.Status == AuditStatusSkipped {
				found = true
			}
		}
		if !found {
			t.Errorf("应标记 secret 跳过：%+v", findings)
		}
	})
}

// getJSON：403 包装为 PermissionError、非 2xx 带响应体、JSON 解析失败带路径。
func TestClientGetJSONBranches(t *testing.T) {
	respond := func(status int, body string) *Client {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(body))
		}))
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	t.Run("403 是 PermissionError", func(t *testing.T) {
		client := respond(http.StatusForbidden, ``)
		status, err := client.getJSON(t.Context(), "/api/v1/x", &struct{}{})
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
		if !IsPermissionError(err) {
			t.Errorf("err = %v (%T), want PermissionError", err, err)
		}
		var permissionErr *PermissionError
		if errors.As(err, &permissionErr) && !strings.Contains(permissionErr.Operation, "/api/v1/x") {
			t.Errorf("Operation = %q, want 含路径", permissionErr.Operation)
		}
	})

	t.Run("非 2xx 带响应体", func(t *testing.T) {
		client := respond(http.StatusBadGateway, `{"message":"upstream down"}`)
		status, err := client.getJSON(t.Context(), "/api/v1/x", &struct{}{})
		if status != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", status)
		}
		if err == nil || !strings.Contains(err.Error(), "upstream down") {
			t.Errorf("err = %v, want 含响应体", err)
		}
	})

	t.Run("JSON 解析失败带路径", func(t *testing.T) {
		client := respond(http.StatusOK, `{not json`)
		if _, err := client.getJSON(t.Context(), "/api/v1/x", &struct{}{}); err == nil ||
			!strings.Contains(err.Error(), "/api/v1/x") {
			t.Errorf("err = %v, want 含路径的解析错误", err)
		}
	})

	t.Run("out 为 nil 时不解析", func(t *testing.T) {
		client := respond(http.StatusOK, `{not json`)
		if _, err := client.getJSON(t.Context(), "/api/v1/x", nil); err != nil {
			t.Errorf("err = %v, want nil（out 为 nil 跳过解析）", err)
		}
	})

	t.Run("空响应体不解析", func(t *testing.T) {
		client := respond(http.StatusOK, ``)
		var out struct {
			A int `json:"a"`
		}
		out.A = 7
		if _, err := client.getJSON(t.Context(), "/api/v1/x", &out); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if out.A != 7 {
			t.Errorf("out 被改写 = %+v", out)
		}
	})
}
