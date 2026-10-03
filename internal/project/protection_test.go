package project

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

type protectionFixture struct {
	defaultBranch        string
	rules                map[string]map[string]any
	requests             []string
	failMethod, failPath string
	deleteMissing        bool
}

func newProtectionFixture(t *testing.T) (*protectionFixture, *Client) {
	t.Helper()
	f := &protectionFixture{defaultBranch: "main", rules: map[string]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		f.requests = append(f.requests, r.Method+" "+path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == f.failMethod && path == f.failPath {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"拒绝"}`)
			return
		}
		const base = "/api/v1/repos/team/repo"
		if path == base && r.Method == "GET" {
			fmt.Fprintf(w, `{"default_branch":%q}`, f.defaultBranch)
			return
		}
		const prefix = base + "/branch_protections"
		branch := strings.TrimPrefix(path, prefix+"/")
		switch {
		case path == prefix && r.Method == "POST":
			var body map[string]any
			if err := json.UnmarshalRead(r.Body, &body); err != nil {
				t.Error(err)
			}
			branch = body["rule_name"].(string)
			f.rules[branch] = body
		case strings.HasPrefix(path, prefix+"/") && r.Method == "GET":
			if f.rules[branch] == nil {
				w.WriteHeader(404)
				return
			}
		case strings.HasPrefix(path, prefix+"/") && r.Method == "PATCH":
			var body map[string]any
			if err := json.UnmarshalRead(r.Body, &body); err != nil {
				t.Error(err)
			}
			for key, val := range body {
				if val != nil {
					f.rules[branch][key] = val
				}
			}
		case strings.HasPrefix(path, prefix+"/") && r.Method == "DELETE":
			delete(f.rules, branch)
			if f.deleteMissing {
				w.WriteHeader(404)
			} else {
				w.WriteHeader(204)
			}
			return
		default:
			t.Errorf("未预期请求 %s %s", r.Method, path)
			w.WriteHeader(500)
			return
		}
		if err := json.MarshalWrite(w, f.rules[branch]); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	c, err := NewClient(credentials.Gitea{URL: server.URL, Username: "admin", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestProtectionLifecycleAndOtherRulesPreserved(t *testing.T) {
	f, c := newProtectionFixture(t)
	f.rules["release/*"] = map[string]any{"rule_name": "release/*", "required_approvals": float64(1)}
	opt := ProtectionOptions{Owner: "team", Name: "repo", Merger: "merge", Checks: []string{"build", "test"}}
	state, err := c.GetProtection(t.Context(), opt)
	if err != nil || state.Branch != "main" || state.Rule != nil {
		t.Fatal(state, err)
	}
	for range 2 {
		if _, err := c.UpdateProtection(t.Context(), opt); err != nil {
			t.Fatal(err)
		}
	}
	state, err = c.GetProtection(t.Context(), opt)
	if err != nil {
		t.Fatal(err)
	}
	r := state.Rule
	if r.RequiredApprovals != 2 || r.EnablePush || !r.EnableMergeWhitelist || !r.BlockAdminMergeOverride || !r.DismissStaleApprovals || !r.BlockOnRejectedReviews || !r.BlockOnOfficialReviewRequests || !r.BlockOnOutdatedBranch || !r.EnableStatusCheck || !reflect.DeepEqual(r.MergeWhitelistUsernames, []string{"merge"}) || !reflect.DeepEqual(r.StatusCheckContexts, []string{"build", "test"}) {
		t.Fatal(r)
	}
	opt.Checks = nil
	if _, err := c.UpdateProtection(t.Context(), opt); err != nil {
		t.Fatal(err)
	}
	state, _ = c.GetProtection(t.Context(), opt)
	if !reflect.DeepEqual(state.Rule.StatusCheckContexts, []string{"build", "test"}) {
		t.Fatal("未指定检查时丢失旧配置")
	}
	opt.Checks = []string{}
	if _, err := c.UpdateProtection(t.Context(), opt); err != nil {
		t.Fatal(err)
	}
	state, _ = c.GetProtection(t.Context(), opt)
	if state.Rule.EnableStatusCheck || len(state.Rule.StatusCheckContexts) != 0 {
		t.Fatal("没有清空必要检查", state.Rule)
	}
	for range 2 {
		if _, err := c.RemoveProtection(t.Context(), opt); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.rules) != 1 || f.rules["release/*"] == nil {
		t.Fatal("移除影响其他规则", f.rules)
	}
}

func TestProtectionExplicitBranchAndUnmanagedFields(t *testing.T) {
	f, c := newProtectionFixture(t)
	f.defaultBranch = ""
	f.rules["feature/demo"] = map[string]any{"rule_name": "feature/demo", "require_signed_commits": true, "protected_file_patterns": "secrets/**", "status_check_contexts": []string{"build"}, "merge_whitelist_teams": []string{"other"}}
	opt := ProtectionOptions{Owner: "team", Name: "repo", Branch: "feature/demo", Merger: "merge"}
	if _, err := c.UpdateProtection(t.Context(), opt); err != nil {
		t.Fatal(err)
	}
	state, err := c.GetProtection(t.Context(), opt)
	if err != nil || !state.Rule.RequireSignedCommits || state.Rule.ProtectedFilePatterns != "secrets/**" || len(state.Rule.MergeWhitelistTeams) != 0 {
		t.Fatal(state, err)
	}
	if _, err := c.RemoveProtection(t.Context(), opt); err != nil {
		t.Fatal(err)
	}
	if f.rules[opt.Branch] != nil {
		t.Fatal("没有删除含斜杠的分支规则")
	}
}

func TestProtectionDryRunIsReadOnly(t *testing.T) {
	for _, existing := range []bool{false, true} {
		f, c := newProtectionFixture(t)
		if existing {
			f.rules["main"] = map[string]any{"rule_name": "main", "required_approvals": float64(1)}
		}
		before, _ := json.Marshal(f.rules, json.Deterministic(true))
		opt := ProtectionOptions{Owner: "team", Name: "repo", Merger: "merge", DryRun: true}
		if _, err := c.UpdateProtection(t.Context(), opt); err != nil {
			t.Fatal(err)
		}
		if _, err := c.RemoveProtection(t.Context(), opt); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(f.rules, json.Deterministic(true))
		if string(before) != string(after) {
			t.Fatal("演练改变规则")
		}
		for _, request := range f.requests {
			if !strings.HasPrefix(request, "GET ") {
				t.Fatal("演练产生写请求", request)
			}
		}
	}
}

func TestProtectionErrorsAndMismatchedRulesDoNotWrite(t *testing.T) {
	for _, scenario := range []string{"invalid-repo", "invalid-branch", "invalid-default", "repo-read", "protection-read", "invalid-merger", "invalid-check", "create", "edit", "remove", "remove-read", "mismatched-update", "mismatched-remove", "delete-race"} {
		t.Run(scenario, func(t *testing.T) {
			f, c := newProtectionFixture(t)
			opt := ProtectionOptions{Owner: "team", Name: "repo", Merger: "merge"}
			switch scenario {
			case "invalid-repo":
				opt.Owner = ".."
			case "invalid-branch":
				opt.Branch = "release/*"
			case "invalid-default":
				f.defaultBranch = ""
			case "repo-read":
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo"
			case "protection-read", "remove-read":
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			case "invalid-merger":
				opt.Merger = "../merge"
			case "invalid-check":
				opt.Checks = []string{" "}
			case "create":
				f.failMethod = "POST"
				f.failPath = "/api/v1/repos/team/repo/branch_protections"
			case "edit":
				f.rules["main"] = map[string]any{"rule_name": "main"}
				f.failMethod = "PATCH"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			case "remove":
				f.rules["main"] = map[string]any{"rule_name": "main"}
				f.failMethod = "DELETE"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			case "mismatched-update", "mismatched-remove":
				f.rules["main"] = map[string]any{"rule_name": "*"}
			case "delete-race":
				f.rules["main"] = map[string]any{"rule_name": "main"}
				f.deleteMissing = true
			}
			var err error
			if scenario == "remove" || scenario == "remove-read" || scenario == "mismatched-remove" || scenario == "delete-race" {
				_, err = c.RemoveProtection(t.Context(), opt)
			} else {
				_, err = c.UpdateProtection(t.Context(), opt)
			}
			if scenario == "delete-race" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("错误被忽略")
			}
			if strings.HasPrefix(scenario, "mismatched") || strings.HasPrefix(scenario, "invalid") || strings.HasSuffix(scenario, "read") {
				for _, request := range f.requests {
					if !strings.HasPrefix(request, "GET ") {
						t.Fatal("预检失败仍写入", request)
					}
				}
			}
		})
	}
}

func TestProtectionRejectsPatternsAndInvalidRefNames(t *testing.T) {
	for _, branch := range []string{"", "@", "HEAD", "-main", "a b", "a\tb", "a\x7f", "a\x00", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "a..b", "a@{b", "a.", "/a", "a/", "a//b", ".a", "a/.b", "a.lock", "a/b.lock"} {
		if validProtectionBranch(branch) {
			t.Fatal("接受无效分支", branch)
		}
	}
	for _, branch := range []string{"main", "feature/demo", "修复/缺陷", "v1.0", "a_b-c"} {
		if !validProtectionBranch(branch) {
			t.Fatal("拒绝有效分支", branch)
		}
	}
}
