package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// 多通道接线：agent 池解析、qq 通道启动、weixin 未配置的说明性跳过——一次
// startDaemonServices 全部说清。
func TestStartDaemonServicesStartsChannels(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{
		Agents: map[string]instances.Agent{
			"ops": {Model: "test-model"},
		},
		DefaultAgent: "ops",
		QQ: &instances.QQ{
			Enabled:    true,
			AppID:      "app-1",
			AppSecret:  "sec-1",
			APIBaseURL: "http://127.0.0.1:1", // 不可达：通道保持重试，不影响启动
			AdminUsers: []string{"*"},
			Agent:      "ops",
		},
	}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := &cobra.Command{}
	// 通道协程在 cancel 后仍可能补写日志：用并发安全的缓冲读
	output := &safeBuffer{}
	command.SetContext(ctx)
	command.SetOut(output)
	command.SetErr(&safeBuffer{})
	store := daemon.NewStore("test-version")
	if err := startDaemonServices(command, configPath, &dispatcherOptions{APIListen: "none"}, store, nil); err != nil {
		t.Fatalf("startDaemonServices: %v", err)
	}
	cancel()

	logs := output.String()
	for _, want := range []string{
		"agent 池 = ops（默认 ops",
		"通道 qq 已启动",
		"通道 weixin 未启用",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺少 %q：\n%s", want, logs)
		}
	}
}

// safeBuffer 是并发安全的字节缓冲（cancel 后的通道协程日志与测试读取并发）。
type safeBuffer struct {
	mu  sync.Mutex
	raw bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.raw.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.raw.String()
}
