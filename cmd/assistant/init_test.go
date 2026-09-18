package main

import (
	"context"
	"os"
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

// 空骨架（config new 产物）在标准位置时按「没有配置」处理：退回环境变量
// 单实例模式——token 已备好的设备上 export GITEA_HOST/GITEA_ACCESS_TOKEN
// 即可跑 run，填好 instances 后配置文件自然接管。
func TestResolveInstanceFileTreatsSkeletonAsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ASSISTANT_CONFIG", "")
	configDir := filepath.Join(dir, "Cosmic-Developers-Union", "assistant")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skeleton := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(skeleton, []byte("{\n  \"$schema\": \"./config.schema.json\",\n  \"instances\": []\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, file, err := resolveInstanceFile(commandOptions{})
	if err != nil {
		t.Fatalf("空骨架应退回 env 模式而不是报错: %v", err)
	}
	if path != "" || file != nil {
		t.Errorf("空骨架应视为无配置文件: path=%q file=%v", path, file)
	}
}

// 骨架被填过（instances 非空）后恢复配置文件语义；坏文件仍然报错。
func TestResolveInstanceFileReportsRealProblems(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ASSISTANT_CONFIG", "")
	configDir := filepath.Join(dir, "Cosmic-Developers-Union", "assistant")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	filled := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(filled, []byte(`{"instances":[{"host":"https://g.example","reviewer":{"name":"a"},"merger":{"name":"b"},"repos":["o/r"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, file, err := resolveInstanceFile(commandOptions{})
	if err != nil || file == nil {
		t.Fatalf("填好的配置应正常加载: err=%v file=%v", err, file)
	}
	if path != filled || len(file.Instances) != 1 {
		t.Errorf("应加载填好的配置: path=%q instances=%d", path, len(file.Instances))
	}

	if err := os.WriteFile(filled, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveInstanceFile(commandOptions{}); err == nil {
		t.Error("坏 JSON 应当报错")
	}
}
