package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

// 未 setup 的实例（无 repos）应跳过并提示，不阻断其他实例。
func TestResolveDispatchTargetsSkipsInstancesWithoutRepos(t *testing.T) {
	dir := t.TempDir()
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://gitea.aicler.com", AdminToken: "token-aicler"},
		{
			Host:       "https://gitea.mms.vincentge.top",
			AdminToken: "token-mms",
			Repos:      []instances.Repo{{Name: "Ge/assistant", Dir: dir}},
		},
	}}
	file.Normalize()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, file); err != nil {
		t.Fatalf("save config: %v", err)
	}

	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	targets, err := resolveDispatchTargets(command, "", path, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveDispatchTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].config.Repository.FullName() != "Ge/assistant" {
		t.Fatalf("targets = %+v, want 仅 Ge/assistant", targets)
	}
	if !strings.Contains(stderr.String(), "跳过 https://gitea.aicler.com") {
		t.Errorf("stderr = %q, 缺少跳过提示", stderr.String())
	}
}
