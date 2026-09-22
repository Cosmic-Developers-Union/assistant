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
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{
		Agents: map[string]instances.Agent{
			"ops": {Model: "test-model"},
		},
		DefaultAgent: "ops",
		Channels: []instances.Channel{{
			Type:       instances.ChannelQQ,
			AppID:      "app-1",
			AppSecret:  "sec-1",
			APIBaseURL: "http://127.0.0.1:1", // 不可达：通道保持重试，不影响启动
			AdminUsers: []string{"*"},
		}},
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
		"runtime 装配：main agent = main", // 缺省主 agent（用户 agents.ops 覆盖内置同名预设）
		"子代理     = coder、ops、review、writer",
		"通道 qq 已启动",
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

// channels 多实例接线：命名 weixin + 命名 qq + telegram 一次起齐，内置 agent
// 引用直接生效。端点指向不可达地址，只验证启动面与日志。
func TestStartDaemonServicesStartsChannelList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.json")
	dead := "http://127.0.0.1:1"
	file := &instances.File{
		Channels: []instances.Channel{
			{Type: instances.ChannelWeixin, Name: "work", BaseURL: dead, BotToken: "wx", Agent: "ops"},
			{Type: instances.ChannelQQ, Name: "support", APIBaseURL: dead, AppID: "1", AppSecret: "s"},
			{Type: instances.ChannelTelegram, APIBaseURL: dead, BotToken: "123:abc"},
		},
	}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := &cobra.Command{}
	output := &safeBuffer{}
	command.SetContext(ctx)
	command.SetOut(output)
	command.SetErr(&safeBuffer{})
	if err := startDaemonServices(command, configPath, &dispatcherOptions{APIListen: "none"}, daemon.NewStore("test-version"), nil); err != nil {
		t.Fatalf("startDaemonServices: %v", err)
	}
	cancel()

	logs := output.String()
	for _, want := range []string{
		"通道 weixin/work 已启动",
		"通道 qq/support 已启动",
		"通道 telegram 已启动",
		// 无 runtimes 时即时合成 main：全部通道 + 内置 main agent
		"通道 = weixin/work、qq/support、telegram",
		"main agent = main（provider 内置缺省",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺少 %q：\n%s", want, logs)
		}
	}
}
