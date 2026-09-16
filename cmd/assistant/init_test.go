package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

func TestResolveRepoSetupTargetUsesPlatformByHost(t *testing.T) {
	dir := gitRemoteFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{{
		Host: "http://gitea.example.com:3000",
	}}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	probe := func(host string) bool { return host == "http://gitea.example.com:3000" }
	target, err := resolveRepoSetupTargetWithProbe(
		context.Background(), &cobra.Command{}, configPath, nil, probe)
	if err != nil {
		t.Fatalf("resolveRepoSetupTarget() error = %v", err)
	}
	if target.FullName != "acme/repo" || target.Host != "http://gitea.example.com:3000" || target.Path != configPath {
		t.Fatalf("target = %+v", target)
	}
	if target.Instance.Host != "http://gitea.example.com:3000" {
		t.Errorf("instance = %+v", target.Instance)
	}
}

func TestResolveRepoSetupTargetRequiresConfig(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := gitRemoteFixture(t)
	t.Chdir(dir)
	_, err := resolveRepoSetupTarget(context.Background(), &cobra.Command{}, "", []string{"acme/repo"})
	if err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("error = %v, want 缺少 config.json", err)
	}
}
