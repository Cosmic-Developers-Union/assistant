package status

import (
	"encoding/json"
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
	fixture.labels = fixture.labels[:len(fixture.labels)-1] // 少一个标签
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
	if len(findings) != 1 || findings[0].Status != AuditStatusSkipped || !findings[0].OK() {
		t.Errorf("权限不足时应跳过而非报错: %+v", findings)
	}
}
