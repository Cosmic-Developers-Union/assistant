package runcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMinimalUsesPlatformDefaults(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := writeTemp(t, FileName, `
monitor:
  http://gitea.example:1234:
    token: abc
    repos:
      - user/repo1
      - user/repo2
`)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data := os.Getenv("XDG_DATA_HOME")
	if file.DataRoot() != filepath.Join(data, "Cosmic-Developers-Union", "assistant") {
		t.Errorf("DataRoot = %q", file.DataRoot())
	}
	if file.ReposRoot() != filepath.Join(file.DataRoot(), "repos") {
		t.Errorf("ReposRoot = %q", file.ReposRoot())
	}
	if file.ReviewRootDir() != os.TempDir() {
		t.Errorf("ReviewRootDir = %q", file.ReviewRootDir())
	}
	name, err := file.ReviewName("http://gitea.example:1234", "user/repo1", "pr", 42)
	if err != nil {
		t.Fatalf("ReviewName: %v", err)
	}
	if name != "gitea.example-1234-user--repo1-pr-42" {
		t.Errorf("ReviewName = %q", name)
	}
}

func TestLoadFullExample(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	path := writeTemp(t, FileName, `
monitor:
  http://gitea.example:1234:
    token: t1
    repos:
      - user/repo1
  https://gitea.other:
    token: t2
    repos:
      - acme/rocket
root: ${XDG_DATA_HOME:-$HOME/.local/share}/Cosmic-Developers-Union/assistant
repos-dir: $root/repos
review-root: /tmp
review-name-template: "${instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"
`)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.DataRoot() != filepath.Join(data, "Cosmic-Developers-Union", "assistant") {
		t.Errorf("DataRoot = %q", file.DataRoot())
	}
	if file.ReposRoot() != filepath.Join(file.DataRoot(), "repos") {
		t.Errorf("ReposRoot = %q", file.ReposRoot())
	}
	if file.ReviewRootDir() != "/tmp" {
		t.Errorf("ReviewRootDir = %q", file.ReviewRootDir())
	}
	repoPath, err := file.TargetPath("https://gitea.other", "acme/rocket")
	if err != nil {
		t.Fatalf("TargetPath: %v", err)
	}
	want := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "repos", "gitea.other", "acme", "rocket")
	if repoPath != want {
		t.Errorf("TargetPath = %q, want %q", repoPath, want)
	}
	statePath, err := file.RepoStatePath("https://gitea.other", "acme/rocket")
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	wantState := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "state", "gitea.other", "acme", "rocket")
	if statePath != wantState {
		t.Errorf("StatePath = %q, want %q", statePath, wantState)
	}
}

func TestLoadRootVariableFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("RUNCFG_TEST_ROOT", "")
	t.Setenv("HOME", "/home/tester")
	path := writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
root: ${RUNCFG_TEST_ROOT:-$HOME/.local/share}/assistant
`)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.DataRoot() != "/home/tester/.local/share/assistant" {
		t.Errorf("DataRoot = %q, want 环境变量缺省回退", file.DataRoot())
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
bogus: true
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want 未知字段报错", err)
	}
}

func TestLoadProviderSection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("RUNCFG_TEST_TOKEN", "sk-cp-test")
	t.Setenv("RUNCFG_TEST_EMPTY", "")
	path := writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
# 目前只支持 minimax
provider:
  type: minimax
  token: $RUNCFG_TEST_TOKEN
`)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.Provider == nil || file.Provider.Type != "minimax" || file.Provider.Token != "sk-cp-test" {
		t.Fatalf("Provider = %+v", file.Provider)
	}

	// ${VAR} 花括号写法与未定义变量（展开为空，回退进程环境）
	path = writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
provider:
  type: minimax-cn
  token: ${RUNCFG_TEST_EMPTY}
`)
	file, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.Provider.Type != "minimax-cn" || file.Provider.Token != "" {
		t.Fatalf("Provider = %+v, want 空令牌（回退环境）", file.Provider)
	}

	// 未闭合的引用报错；未知预设报错
	path = writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
provider:
  type: minimax
  token: ${OPEN
`)
	if _, err := Load(path); err == nil {
		t.Error("token 未闭合引用应报错")
	}
	path = writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
provider:
  type: nosuch
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "内置预设") {
		t.Fatalf("err = %v, want 未知预设报错", err)
	}
	path = writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    repos: [a/b]
provider:
  token: abc
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "provider.type") {
		t.Fatalf("err = %v, want 缺 type 报错", err)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"monitor 为空":     "\nroot: /tmp\n",
		"站点地址非法":         "\nmonitor:\n  gitea.example:\n    repos: [a/b]\n",
		"仓库格式非法":         "\nmonitor:\n  https://gitea.example:\n    repos: [onlyname]\n",
		"root 未闭合变量引用":   "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: ${OPEN\n",
		"root 空变量名":      "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: ${:-/tmp}\n",
	}
	for label, content := range cases {
		path := writeTemp(t, FileName, content)
		if _, err := Load(path); err == nil {
			t.Errorf("%s：应报错", label)
		}
	}
}

func TestReviewNamePlaceholders(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := writeTemp(t, FileName, `
monitor:
  http://gitea.example:1234:
    repos: [user/repo1]
review-name-template: "${instance-name}/{username-or-org}/{name}-{pr|issue}-{index}"
`)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pr, err := file.ReviewName("http://gitea.example:1234", "user/repo1", "pr", 7)
	if err != nil {
		t.Fatalf("ReviewName: %v", err)
	}
	if pr != "gitea.example-1234/user/repo1-pr-7" {
		t.Errorf("pr name = %q", pr)
	}
	issue, err := file.ReviewName("http://gitea.example:1234", "user/repo1", "issue", 9)
	if err != nil {
		t.Fatalf("ReviewName: %v", err)
	}
	if issue != "gitea.example-1234/user/repo1-issue-9" {
		t.Errorf("issue name = %q", issue)
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ASSISTANT_RUN", "")
	// 不存在：返回空串（回退 config.json 语义），不报错
	path, err := ResolvePath("", "")
	if err != nil || path != "" {
		t.Fatalf("ResolvePath = %q, %v", path, err)
	}
	// config.json 同目录（随配置目录挂载）：优先于标准落点
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	beside := filepath.Join(configDir, FileName)
	if err := os.WriteFile(beside, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err = ResolvePath("", configPath)
	if err != nil || path != beside {
		t.Fatalf("ResolvePath = %q, %v, want %q", path, err, beside)
	}
	// 标准落点存在：自动发现
	standard, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(standard), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(standard, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err = ResolvePath("", "")
	if err != nil || path != standard {
		t.Fatalf("ResolvePath = %q, %v, want %q", path, err, standard)
	}
	// 显式参数最优先
	path, _ = ResolvePath("/elsewhere/run.yaml", "")
	if path != "/elsewhere/run.yaml" {
		t.Fatalf("ResolvePath = %q", path)
	}
}
