package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// 旧 run.yaml 导入：运行参数进 runtime、monitor 并入 gitea 通道（token/repos）、
// provider 简写转 providers + default_provider；旧文件改名 .migrated。
func TestConfigMigrateImportsRunYaml(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	runPath := filepath.Join(dir, "run.yaml")
	runYaml := `
monitor:
  https://gitea.example.com:
    token: legacy-token
    repos:
      - acme/rocket
      - acme/lab
root: /srv/assistant
sessions-dir: /srv/assistant/sessions
provider:
  type: minimax
  token: legacy-ai-token
`
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runPath, []byte(runYaml), 0o600); err != nil {
		t.Fatal(err)
	}

	command := newConfigMigrateCommand(&configPath)
	output := &bytes.Buffer{}
	command.SetOut(output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--run", runPath})
	if err := command.Execute(); err != nil {
		t.Fatalf("config migrate: %v\n%s", err, output.String())
	}

	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatalf("迁移后的配置无法加载：%v", err)
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if runtime.Root != "/srv/assistant" || runtime.SessionsDir != "/srv/assistant/sessions" {
		t.Errorf("运行参数未导入：root=%q sessions=%q", runtime.Root, runtime.SessionsDir)
	}
	channel, ok := findGiteaChannel(file, "https://gitea.example.com")
	if !ok {
		t.Fatalf("monitor 未并入 gitea 通道：%+v", file.Channels)
	}
	if channel.Token != "legacy-token" {
		t.Errorf("token 未导入：%q", channel.Token)
	}
	names := make([]string, 0, len(channel.Repos))
	for _, repo := range channel.Repos {
		names = append(names, repo.Name)
	}
	if strings.Join(names, ",") != "acme/rocket,acme/lab" {
		t.Errorf("repos 未导入：%v", names)
	}
	if file.DefaultProvider != "minimax" || file.Providers["minimax"].Token() != "legacy-ai-token" {
		t.Errorf("provider 简写未导入：default=%q provider=%+v", file.DefaultProvider, file.Providers["minimax"])
	}
	if _, err := os.Stat(runPath); !os.IsNotExist(err) {
		t.Errorf("旧 run.yaml 应改名 .migrated：%v", err)
	}
	if _, err := os.Stat(runPath + ".migrated"); err != nil {
		t.Errorf("缺少 .migrated 备份：%v", err)
	}
	for _, want := range []string{"+ runtimes.main.root", "+ channels[gitea].repos acme/rocket", "+ default_provider = minimax"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, output.String())
		}
	}
}

// 没有 run.yaml：提示无需迁移，不改任何文件。
func TestConfigMigrateWithoutRunYaml(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := newConfigMigrateCommand(&configPath)
	output := &bytes.Buffer{}
	command.SetOut(output)
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatalf("config migrate: %v", err)
	}
	if !strings.Contains(output.String(), "无需迁移") {
		t.Errorf("输出 = %s", output.String())
	}
}
