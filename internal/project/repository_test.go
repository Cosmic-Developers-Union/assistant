package project

import (
	"strings"
	"testing"
)

func repoOptions() RepositoryOptions {
	return RepositoryOptions{Owner: "team", Name: "repo", Reviewer: "ai", Merger: "merge"}
}
func TestConfigureEnforcesCollaboratorsAndDualApprovalProtection(t *testing.T) {
	for _, existing := range []bool{false, true} {
		f := newFixture()
		f.protection = existing
		client := f.client(t)
		opt := repoOptions()
		if !existing {
			opt.Checks = []string{"build"}
		}
		if err := client.Configure(t.Context(), opt); err != nil {
			t.Fatal(err)
		}
		var protect map[string]any
		reviewer, merger := false, false
		for i, body := range f.bodies {
			if body["permission"] == "write" {
				reviewer = true
			}
			if body["permission"] == "admin" {
				merger = true
			}
			if _, ok := body["required_approvals"]; ok {
				protect = f.bodies[i]
			}
		}
		if !reviewer || !merger || protect["required_approvals"] != float64(2) || protect["block_admin_merge_override"] != true || protect["enable_push"] != false || protect["dismiss_stale_approvals"] != true || protect["enable_merge_whitelist"] != true {
			t.Fatal(f.bodies)
		}
		whitelist := protect["merge_whitelist_usernames"].([]any)
		if len(whitelist) != 1 || whitelist[0] != "merge" {
			t.Fatal(whitelist)
		}
		checks := protect["status_check_contexts"].([]any)
		want := "build"
		if existing {
			want = "test"
		}
		if len(checks) != 1 || checks[0] != want {
			t.Fatal("检查上下文未保留", checks)
		}
	}
}
func TestConfigureDryRunDoesNotWrite(t *testing.T) {
	f := newFixture()
	client := f.client(t)
	opt := repoOptions()
	opt.DryRun = true
	if err := client.Configure(t.Context(), opt); err != nil {
		t.Fatal(err)
	}
	for _, req := range f.requests {
		if !strings.HasPrefix(req, "GET ") {
			t.Fatal("演练写入", req)
		}
	}
}
func TestConfigureReportsEachFailureBoundary(t *testing.T) {
	for _, scenario := range []string{"invalid", "invalid-repo", "repo", "empty-branch", "labels", "create-label", "reviewer", "merger", "protection-read", "protection-create", "protection-edit"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture()
			client := f.client(t)
			opt := repoOptions()
			switch scenario {
			case "invalid":
				opt.Merger = opt.Reviewer
			case "invalid-repo":
				opt.Owner = ".."
			case "repo":
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo"
			case "empty-branch":
				f.defaultBranch = ""
			case "labels":
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo/labels"
			case "create-label":
				f.failMethod = "POST"
				f.failPath = "/api/v1/repos/team/repo/labels"
			case "reviewer":
				f.failMethod = "PUT"
				f.failPath = "/api/v1/repos/team/repo/collaborators/ai"
			case "merger":
				f.failMethod = "PUT"
				f.failPath = "/api/v1/repos/team/repo/collaborators/merge"
			case "protection-read":
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			case "protection-create":
				f.failMethod = "POST"
				f.failPath = "/api/v1/repos/team/repo/branch_protections"
			case "protection-edit":
				f.protection = true
				f.failMethod = "PATCH"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			}
			if err := client.Configure(t.Context(), opt); err == nil {
				t.Fatal("失败被吞掉")
			}
		})
	}
}
func TestActionsSettingsInstallAndDryRun(t *testing.T) {
	for _, scenario := range []string{"ok", "dry", "missing-token", "enable-failure", "secret-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture()
			client := f.client(t)
			token := "merge-token"
			dry := scenario == "dry"
			switch scenario {
			case "missing-token":
				token = ""
			case "enable-failure":
				f.failMethod = "PATCH"
				f.failPath = "/api/v1/repos/team/repo"
			case "secret-failure":
				f.failMethod = "PUT"
				f.failPath = "/api/v1/repos/team/repo/actions/secrets/MERGE_TOKEN"
			}
			err := client.InstallActionsSettings(t.Context(), "team", "repo", token, dry)
			if scenario == "ok" || scenario == "dry" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("失败被吞掉")
			}
			if dry && len(f.requests) != 0 {
				t.Fatal("演练请求平台", f.requests)
			}
			if scenario == "ok" {
				if len(f.bodies) != 2 || f.bodies[0]["has_actions"] != true || f.bodies[1]["data"] != "merge-token" {
					t.Fatal(f.bodies)
				}
			}
		})
	}
}
