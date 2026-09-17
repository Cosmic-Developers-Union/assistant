package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/daemon"
	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

// run 的 daemon 服务面：状态 API 起来后端点可自举发现，MCP/对话都用它。
func TestStartDaemonServicesServesAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.json")
	savePlatform(t, configPath, instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
		Repos:    []instances.Repo{{Name: "acme/repo"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := &cobra.Command{}
	command.SetContext(ctx)
	command.SetOut(&bytes.Buffer{})
	store := daemon.NewStore("test-version")
	store.AddTarget(daemon.Target{Host: "https://gitea.example.com", Repository: "acme/repo"})
	if err := startDaemonServices(command, configPath, &dispatcherOptions{APIListen: "127.0.0.1:0"}, store, nil); err != nil {
		t.Fatalf("startDaemonServices: %v", err)
	}

	client, err := daemon.Discover(os.Getenv)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Version != "test-version" || len(status.Targets) != 1 {
		t.Errorf("status = %+v", status)
	}
	if output := command.OutOrStdout().(*bytes.Buffer).String(); !strings.Contains(output, "状态 API 监听") {
		t.Errorf("日志缺少 API 监听提示：%s", output)
	}
}
