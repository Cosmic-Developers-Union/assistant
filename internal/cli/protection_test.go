package cli

import (
	json "encoding/json/v2"
	"os/exec"
	"strings"
	"testing"
)

func TestProtectionCommandLifecycleAndAliases(t *testing.T) {
	f := projectTestFixture(t)
	args := []string{"project", "--dir", f.dir, "protection"}
	run := func(rest ...string) string {
		t.Helper()
		out, _, err := command(t, append(append([]string{}, args...), rest...)...)
		if err != nil {
			t.Fatal(rest, err)
		}
		return out
	}
	if out := run("show", "--instance", "admin"); !strings.Contains(out, "无同名规则") {
		t.Fatal(out)
	}
	if out := run("install", "--instance", "admin", "--required-checks", "build,test", "--dry-run"); !strings.Contains(out, "演练") || f.writes != 0 {
		t.Fatal(out, f.writes)
	}
	run("install", "--instance", "admin", "--required-checks", "build,test")
	if f.writes != 1 {
		t.Fatal("更新修改了分支保护以外的状态", f.writes)
	}
	out := run("show", "--instance", "admin")
	for _, want := range []string{"分支：main", "批准数：2", "允许直接推送：false", "合并账号：merge", "管理员禁止绕过：true", "必要检查：build,test"} {
		if !strings.Contains(out, want) {
			t.Fatal(out, want)
		}
	}
	out = run("show", "--instance", "admin", "--json")
	var state struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Rule       struct {
			Checks    []string `json:"status_check_contexts"`
			Enabled   bool     `json:"enable_status_check"`
			Approvals int      `json:"required_approvals"`
		} `json:"rule"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil || state.Repository != "team/repo" || state.Branch != "main" || state.Rule.Approvals != 2 || len(state.Rule.Checks) != 2 {
		t.Fatal(out, err)
	}
	run("update", "--instance", "admin")
	out = run("show", "--instance", "admin", "--json")
	_ = json.Unmarshal([]byte(out), &state)
	if len(state.Rule.Checks) != 2 {
		t.Fatal("缺省丢失必要检查", out)
	}
	run("update", "--instance", "admin", "--required-checks=")
	out = run("show", "--instance", "admin", "--json")
	_ = json.Unmarshal([]byte(out), &state)
	if len(state.Rule.Checks) != 0 || state.Rule.Enabled {
		t.Fatal("检查未清空", out)
	}
	writes := f.writes
	run("remove", "--instance", "admin", "--dry-run")
	if f.writes != writes || f.protection == nil {
		t.Fatal("演练移除保护")
	}
	run("uninstall", "--instance", "admin")
	run("remove", "--instance", "admin")
	if f.writes != writes+1 {
		t.Fatal("重复卸载仍发删除请求")
	}
	out = run("show", "--instance", "admin", "--json")
	if !strings.Contains(out, `"rule":null`) {
		t.Fatal(out)
	}
}

func TestProtectionCommandExplicitRepositoryAndBranch(t *testing.T) {
	f := projectTestFixture(t)
	if out, err := exec.Command("git", "-C", f.dir, "remote", "remove", "gitea").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	out, _, err := command(t, "project", "--dir", f.dir, "protection", "update", "--instance", "admin", "--repo", "team/repo", "--branch", "feature/demo", "--merger", "bot-merge")
	if err != nil || !strings.Contains(out, "feature/demo") || !strings.Contains(out, "@bot-merge") || f.writes != 1 {
		t.Fatal(out, err, f.writes)
	}
}

func TestProtectionCommandValidatesSelectionAndReportsErrors(t *testing.T) {
	for _, scenario := range []string{"missing-instance", "unknown-instance", "no-remote", "bad-repo", "bad-branch", "get-failure", "update-failure", "remove-failure", "missing-project"} {
		t.Run(scenario, func(t *testing.T) {
			f := projectTestFixture(t)
			args := []string{"project", "--dir", f.dir, "protection", "update", "--instance", "admin"}
			switch scenario {
			case "missing-instance":
				args = args[:5]
			case "unknown-instance":
				args[len(args)-1] = "missing"
			case "no-remote":
				if out, err := exec.Command("git", "-C", f.dir, "remote", "remove", "gitea").CombinedOutput(); err != nil {
					t.Fatal(string(out), err)
				}
			case "bad-repo":
				args = append(args, "--repo", "../repo")
			case "bad-branch":
				args = append(args, "--branch", "*")
			case "missing-project":
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo"
			case "get-failure":
				args[4] = "show"
				f.failMethod = "GET"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			case "update-failure":
				f.failMethod = "POST"
				f.failPath = "/api/v1/repos/team/repo/branch_protections"
			case "remove-failure":
				args[4] = "remove"
				f.protection = map[string]any{"rule_name": "main"}
				f.failMethod = "DELETE"
				f.failPath = "/api/v1/repos/team/repo/branch_protections/main"
			}
			out, _, err := command(t, args...)
			if err == nil || out != "" || strings.Contains(err.Error(), "token-admin") {
				t.Fatal(out, err)
			}
			if scenario != "update-failure" && scenario != "remove-failure" && f.writes != 0 {
				t.Fatal("预检失败仍写入")
			}
		})
	}
}
