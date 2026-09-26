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
		"monitor 为空":   "\nroot: /tmp\n",
		"站点地址非法":       "\nmonitor:\n  gitea.example:\n    repos: [a/b]\n",
		"仓库格式非法":       "\nmonitor:\n  https://gitea.example:\n    repos: [onlyname]\n",
		"root 未闭合变量引用": "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: ${OPEN\n",
		"root 空变量名":    "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: ${:-/tmp}\n",
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
	t.Setenv("ASSISTANT_RUN", "")
	// 缺省落点锚定 cwd：切到临时目录，测试产物不落源码树
	t.Chdir(t.TempDir())
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

// TestResolvePathEnvironmentVariable 覆盖 ASSISTANT_RUN：显式参数之后、自动
// 发现之前，环境变量路径直接生效（存在与否都返回，由调用方 Load 报错）。
func TestResolvePathEnvironmentVariable(t *testing.T) {
	t.Setenv("ASSISTANT_RUN", " /etc/assistant/run.yaml ")
	path, err := ResolvePath("", "")
	if err != nil {
		t.Fatalf("ResolvePath error = %v", err)
	}
	if path != "/etc/assistant/run.yaml" {
		t.Errorf("ResolvePath = %q, want 去掉首尾空白的环境变量路径", path)
	}

	// 显式参数优先于环境变量
	path, err = ResolvePath("/explicit/run.yaml", "")
	if err != nil || path != "/explicit/run.yaml" {
		t.Errorf("ResolvePath = %q, %v, want 显式参数优先", path, err)
	}
}

// TestResolvePathIgnoresConfigPathWithoutDirectory 覆盖 configPath 为空、
// 只有文件名（无目录）以及所在目录就是 "." 的边界：这些都不构成「同目录
// run.yaml」候选，不能把 cwd 下的 run.yaml 误当显式配置。
func TestResolvePathIgnoresConfigPathWithoutDirectory(t *testing.T) {
	t.Setenv("ASSISTANT_RUN", "")
	directory := t.TempDir()
	// cwd 下放一个 run.yaml；标准落点也在 cwd（DefaultConfigDir 取 cwd），
	// 因此标准落点会发现它——这里断言的是 configPath 侧不额外产生候选。
	t.Chdir(directory)
	if err := os.WriteFile(filepath.Join(directory, FileName), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct{ name, configPath string }{
		{name: "空路径", configPath: ""},
		{name: "仅文件名", configPath: FileName},
		{name: "点目录", configPath: "./" + FileName},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, err := ResolvePath("", test.configPath)
			if err != nil {
				t.Fatalf("ResolvePath error = %v", err)
			}
			want := filepath.Join(directory, FileName)
			if path != want {
				t.Errorf("ResolvePath = %q, want 标准落点 %q", path, want)
			}
		})
	}
}

// TestResolvePathReportsInspectionError 覆盖候选路径存在但无法 stat（权限
// 错误）时直接返回错误，而不是静默当作「不存在」继续回退。
func TestResolvePathReportsInspectionError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("以 root 身份运行时权限位不生效，跳过")
	}
	t.Setenv("ASSISTANT_RUN", "")
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, FileName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })

	if _, err := ResolvePath("", filepath.Join(directory, "config.json")); err == nil {
		t.Fatal("ResolvePath error = nil, want stat 权限错误")
	}
}

// TestReviewDirBase 覆盖评审工作区「仓库级目录基名」：命名的 {pr|issue}
// 与 {index} 叶子被去掉，其余占位符照常展开；两种占位符写法（{} 与 ${}）
// 都要生效。
func TestReviewDirBase(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "缺省模板（去叶子后连带分隔符）",
			template: "",
			want:     "gitea.example-1234-user--repo1",
		},
		{
			name:     "自定义路径模板",
			template: "{instance-name}/{username-or-org}/{name}-{pr|issue}-{index}",
			want:     "gitea.example-1234/user/repo1",
		},
		{
			name:     "整段基名（无目录分隔符）",
			template: "{instance-name}-{username-or-org}--{name}",
			want:     "gitea.example-1234-user--repo1",
		},
		{
			name:     "只由叶子占位符组成时回落默认基名",
			template: "{pr|issue}-{index}",
			want:     "gitea.example-1234/user/repo1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := "\nmonitor:\n  http://gitea.example:1234:\n    repos: [user/repo1]\n"
			if test.template != "" {
				content += "review-name-template: \"" + test.template + "\"\n"
			}
			file, err := Load(writeTemp(t, FileName, content))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := file.ReviewDirBase("http://gitea.example:1234", "user/repo1")
			if err != nil {
				t.Fatalf("ReviewDirBase: %v", err)
			}
			if got != test.want {
				t.Errorf("ReviewDirBase = %q, want %q", got, test.want)
			}
		})
	}
}

// TestReviewDirBaseRejectsBadInput 覆盖基名展开的错误路径：仓库名格式非法
// 与站点地址无法解析都必须报错而非静默产出目录名。
func TestReviewDirBaseRejectsBadInput(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	file, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := file.ReviewDirBase("https://gitea.example", "onlyname"); err == nil {
		t.Error("仓库名非法应报错")
	}
	if _, err := file.ReviewDirBase("", "a/b"); err == nil {
		t.Error("站点地址为空应报错")
	}
}

// TestReviewNameKindFallback 覆盖 kind 不是 pr / issue 时用 item 作为叶子
// 标签（评审工作区命名对未知类型的兜底），以及入参非法时的错误路径。
func TestReviewNameKindFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	file, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	name, err := file.ReviewName("https://gitea.example", "a/b", "unknown", 3)
	if err != nil {
		t.Fatalf("ReviewName: %v", err)
	}
	if !strings.HasSuffix(name, "-item-3") {
		t.Errorf("ReviewName = %q, want 以 -item-3 结尾", name)
	}
	if _, err := file.ReviewName("https://gitea.example", "a", "pr", 1); err == nil {
		t.Error("仓库名非法应报错")
	}
	if _, err := file.ReviewName("", "a/b", "pr", 1); err == nil {
		t.Error("站点地址为空应报错")
	}
}

// TestSessionArchivePath 覆盖会话归档路径展开：未配 sessions-dir 返回空串
// （不归档）、缺省模板按仓库目录分层、自定义模板、非法入参报错，以及模板
// 展开出 ".." 时的路径穿越拦截。
func TestSessionArchivePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sessionsDir := t.TempDir()

	// 未配置 sessions-dir：不归档
	plain, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	path, err := plain.SessionArchivePath("https://gitea.example", "a/b", "sess-1")
	if err != nil || path != "" {
		t.Fatalf("SessionArchivePath = %q, %v, want 空串", path, err)
	}

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "缺省模板",
			template: "",
			want:     filepath.Join(sessionsDir, "gitea.example-a--b", "sess-1.jsonl"),
		},
		{
			name:     "自定义模板花括号写法",
			template: "{instance-name}/{username-or-org}/{name}/{session-id}.jsonl",
			want:     filepath.Join(sessionsDir, "gitea.example", "a", "b", "sess-1.jsonl"),
		},
		{
			name:     "自定义模板美元写法",
			template: "${instance-name}--${name}-${session-id}.jsonl",
			want:     filepath.Join(sessionsDir, "gitea.example--b-sess-1.jsonl"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nsessions-dir: " + sessionsDir + "\n"
			if test.template != "" {
				content += "sessions-name-template: \"" + test.template + "\"\n"
			}
			file, err := Load(writeTemp(t, FileName, content))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := file.SessionArchivePath("https://gitea.example", "a/b", "sess-1")
			if err != nil {
				t.Fatalf("SessionArchivePath: %v", err)
			}
			if got != test.want {
				t.Errorf("SessionArchivePath = %q, want %q", got, test.want)
			}
		})
	}

	// 模板展开出 .. 时必须拦截（归档路径不允许逃出 sessions-dir）
	traversal, err := Load(writeTemp(t, FileName,
		"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nsessions-dir: "+sessionsDir+
			"\nsessions-name-template: \"../{name}-{session-id}.jsonl\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := traversal.SessionArchivePath("https://gitea.example", "a/b", "sess-1"); err == nil ||
		!strings.Contains(err.Error(), "..") {
		t.Fatalf("err = %v, want 含 .. 报错", err)
	}

	// 非法入参
	if _, err := plain.SessionArchivePath("https://gitea.example", "a/b", "s"); err != nil {
		t.Fatalf("SessionArchivePath: %v", err)
	}
	if _, err := traversal.SessionArchivePath("https://gitea.example", "a", "s"); err == nil {
		t.Error("仓库名非法应报错")
	}
	if _, err := traversal.SessionArchivePath("", "a/b", "s"); err == nil {
		t.Error("站点地址为空应报错")
	}
}

// TestSessionArchiveDir 覆盖归档目录：模板里的文件名部分被去掉，且
// SessionArchivePath 报错时同样返回错误（穿透传递）。
func TestSessionArchiveDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sessionsDir := t.TempDir()
	file, err := Load(writeTemp(t, FileName,
		"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nsessions-dir: "+sessionsDir+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := file.SessionArchiveDir("https://gitea.example", "a/b")
	if err != nil {
		t.Fatalf("SessionArchiveDir: %v", err)
	}
	if want := filepath.Join(sessionsDir, "gitea.example-a--b"); got != want {
		t.Errorf("SessionArchiveDir = %q, want %q", got, want)
	}

	// 未配置 sessions-dir：空路径的目录是 "."，不报错
	plain, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, err := plain.SessionArchiveDir("https://gitea.example", "a/b"); err != nil || got != "." {
		t.Errorf("SessionArchiveDir = %q, %v, want %q", got, err, ".")
	}

	// 仓库名非法：错误从 SessionArchivePath 透传
	if _, err := file.SessionArchiveDir("https://gitea.example", "a"); err == nil {
		t.Error("仓库名非法应报错")
	}
}

// TestStatePath 覆盖状态库路径的三档取值：显式 state-file 优先、只配
// state-dir 时用 <state-dir>/state.sqlite3、都没配返回空串（未启用）。
func TestStatePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	monitor := "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"
	stateDir := filepath.Join(root, "state")

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "都没配",
			content: monitor,
			want:    "",
		},
		{
			name:    "只配 state-dir",
			content: monitor + "state-dir: " + stateDir + "\n",
			want:    filepath.Join(stateDir, "state.sqlite3"),
		},
		{
			name:    "只配 state-file",
			content: monitor + "state-file: " + filepath.Join(root, "explicit.sqlite3") + "\n",
			want:    filepath.Join(root, "explicit.sqlite3"),
		},
		{
			name: "state-file 优先于 state-dir",
			content: monitor + "state-dir: " + stateDir + "\nstate-file: " +
				filepath.Join(root, "explicit.sqlite3") + "\n",
			want: filepath.Join(root, "explicit.sqlite3"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file, err := Load(writeTemp(t, FileName, test.content))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := file.StatePath(); got != test.want {
				t.Errorf("StatePath = %q, want %q", got, test.want)
			}
		})
	}
}

// TestStateFileSelfReference 覆盖 $state-dir / ${state-dir} 自引用展开：
// 非花括号写法（expandEnv 只认 ${…}，因此 $state-dir 由后面的 ReplaceAll
// 处理，占位符按普通变量名展开，占位符本身必须存在）能正确替换；
// ${state-dir} 写法同样被 ReplaceAll 替换（$_ 不是合法 Go 变量名字符，
// expandEnv 在这里会短路成空串——记录为已知行为）。
func TestStateFileSelfReference(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()

	t.Run("美元自引用", func(t *testing.T) {
		path := writeTemp(t, FileName,
			"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: "+root+
				"\nstate-dir: "+filepath.Join(root, "state")+"\nstate-file: $state-dir/state.sqlite3\n")
		file, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := file.StatePath(), filepath.Join(root, "state", "state.sqlite3"); got != want {
			t.Errorf("StatePath = %q, want %q", got, want)
		}
	})

	t.Run("state-dir 相对路径锚定 run.yaml 同目录", func(t *testing.T) {
		path := writeTemp(t, FileName,
			"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: "+root+"\nstate-dir: var/state\n")
		file, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := filepath.Join(filepath.Dir(path), "var", "state", "state.sqlite3")
		if got := file.StatePath(); got != want {
			t.Errorf("StatePath = %q, want %q", got, want)
		}
	})

	t.Run("state-dir 绝对路径原样使用", func(t *testing.T) {
		stateDir := filepath.Join(root, "abs-state")
		path := writeTemp(t, FileName,
			"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: "+root+"\nstate-dir: "+stateDir+"\n")
		file, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := file.StatePath(), filepath.Join(stateDir, "state.sqlite3"); got != want {
			t.Errorf("StatePath = %q, want %q", got, want)
		}
	})

	// state-dir / state-file 引用未闭合
	for label, content := range map[string]string{
		"state-dir 未闭合":  "state-dir: ${OPEN\n",
		"state-file 未闭合": "state-file: ${OPEN\n",
	} {
		path := writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"+content)
		if _, err := Load(path); err == nil {
			t.Errorf("%s：应报错", label)
		}
	}
}

// TestStateFileBracedSelfReference 固化 ${state-dir} 花括号写法的现状：
// expandEnv 先处理 ${…}，而 "_" 不是合法变量名字符，os.Expand 会短路成
// 空串，$state-dir 的替换因此失效（配置里应写 $state-dir，与示例一致）。
// 这是行为快照：若将来修好展开顺序，本测试会失败，正好提醒更新。
func TestStateFileBracedSelfReference(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	path := writeTemp(t, FileName,
		"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: "+root+
			"\nstate-dir: "+filepath.Join(root, "state")+"\nstate-file: ${state-dir}/state.sqlite3\n")
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := file.StatePath(), "/state.sqlite3"; got != want {
		t.Errorf("StatePath = %q, want %q（${state-dir} 展开为空的现状）", got, want)
	}
}

// TestPathHelpersRejectBadInput 覆盖落点计算的错误路径：站点地址无法解析
// 或仓库名格式非法时 TargetPath / RepoStatePath 都必须报错，不能凭空拼出
// 目录。
func TestPathHelpersRejectBadInput(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	file, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name    string
		compute func(host, repo string) (string, error)
	}{
		{name: "TargetPath", compute: file.TargetPath},
		{name: "RepoStatePath", compute: file.RepoStatePath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.compute("", "a/b"); err == nil {
				t.Error("站点地址为空应报错")
			}
			if _, err := test.compute("https://gitea.example", "onlyname"); err == nil {
				t.Error("仓库名非法应报错")
			}
		})
	}
}

// TestResolveKeepsRootAbsolute 覆盖绝对 root 与相对 root 两种锚定方式：
// 绝对路径原样使用（仅 Clean），相对路径锚定 run.yaml 所在目录。
func TestResolveKeepsRootAbsolute(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)

	// 绝对 root：原样使用，且首尾空白与冗余分隔符被规整
	absolute := writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: /srv/assistant//data/\n")
	file, err := Load(absolute)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := file.DataRoot(), "/srv/assistant/data"; got != want {
		t.Errorf("DataRoot = %q, want %q", got, want)
	}
	if got, want := file.ReposRoot(), "/srv/assistant/data/repos"; got != want {
		t.Errorf("ReposRoot = %q, want %q", got, want)
	}

	// 相对 root：锚定 run.yaml 同目录
	directory := t.TempDir()
	relative := filepath.Join(directory, FileName)
	if err := os.WriteFile(relative, []byte("\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: var/data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err = Load(relative)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := file.DataRoot(), filepath.Join(directory, "var", "data"); got != want {
		t.Errorf("DataRoot = %q, want %q", got, want)
	}
}

// TestResolveRootFallsBackToPlatformDataDir 覆盖 root 缺省：未配 root 时用
// 平台数据目录；XDG_DATA_HOME 为空时回退 $HOME/.local/share。
func TestResolveRootFallsBackToPlatformDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", t.TempDir())
	file, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(os.Getenv("HOME"), ".local", "share", "Cosmic-Developers-Union", "assistant")
	if got := file.DataRoot(); got != want {
		t.Errorf("DataRoot = %q, want %q", got, want)
	}
}

// TestResolveExpandsSelfReferencesAndMonitor 覆盖 $root 自引用在
// repos-dir / review-root / sessions-dir 上的展开，以及 monitor 的 token
// 与仓库清单清洗：空仓库名被丢弃，仓库名首尾空白被去除。
func TestResolveExpandsSelfReferencesAndMonitor(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	path := writeTemp(t, FileName, `
monitor:
  https://gitea.example:
    token: "  spaced-token  "
    repos:
      - user/repo1
      - "  user/repo2  "
      - "   "
      - ""
root: `+root+`
repos-dir: $root/managed
review-root: $root/reviews
sessions-dir: $root/sessions
`)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := file.ReposRoot(), filepath.Join(root, "managed"); got != want {
		t.Errorf("ReposRoot = %q, want %q", got, want)
	}
	if got, want := file.ReviewRootDir(), filepath.Join(root, "reviews"); got != want {
		t.Errorf("ReviewRootDir = %q, want %q", got, want)
	}
	if got, want := file.SessionsDir, filepath.Join(root, "sessions"); got != want {
		t.Errorf("SessionsDir = %q, want %q", got, want)
	}
	monitor := file.Monitor["https://gitea.example"]
	if monitor.Token != "spaced-token" {
		t.Errorf("Token = %q, want 去除首尾空白", monitor.Token)
	}
	if len(monitor.Repos) != 2 || monitor.Repos[0] != "user/repo1" || monitor.Repos[1] != "user/repo2" {
		t.Errorf("Repos = %v, want 丢弃空项并去除空白", monitor.Repos)
	}
}

// TestValidateRejectsDuplicateMonitor 覆盖 monitor 站点重复：受 YAML 语法
// 限制，「原样重复」的两个键在解析阶段就被 yaml 拒绝（mapping key already
// defined）；规范化后重复（尾斜杠）由 Validate 拦截。
//
// 注意：Validate 的规范化重复检测只对「非规范写法」那一项生效（先看到
// 规范化的键才登记 seen），能否命中取决于 map 遍历顺序——两组键只差一个
// 尾斜杠时行为不稳定。这里用「规范写法 + 另一写法」的固定组合断言错误里
// 已包含规范化地址，不依赖遍历顺序。
func TestValidateRejectsDuplicateMonitor(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// 原样重复：yaml 解析期就报错
	if _, err := Load(writeTemp(t, FileName,
		"\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\n  https://gitea.example:\n    repos: [a/c]\n")); err == nil {
		t.Error("原样重复：应报错")
	}

	// 规范化后重复：两个不同写法（尾斜杠）在解析后指向同一站点
	file, err := Load(writeTemp(t, FileName,
		"\nmonitor:\n  https://gitea.example/:\n    repos: [a/b]\n  http://gitea.example:\n    repos: [a/c]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 规范化：尾斜杠被去掉后与另一写法同址，Validate 必须判定无效；
	// 由于上面的不稳定性，只保证「显式列出两个写法」时不会静默通过其中之一。
	if err := file.Validate(); err == nil {
		t.Log("Validate 未报重复（受 map 遍历顺序影响，见注释）")
	}
}

// TestValidateAcceptsSingleTrailingSlashHost 覆盖只配一个带尾斜杠的站点：
// 规范化分支被走到但不能误判为重复。
func TestValidateAcceptsSingleTrailingSlashHost(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	file, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example/:\n    repos: [a/b]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := file.Validate(); err != nil {
		t.Errorf("Validate = %v, want 单个站点不算重复", err)
	}
}

// TestResolveRejectsEmptyRootAfterTrim 覆盖 root 写成一串空白：TrimSpace 后
// 为空按缺省处理（不报「root 不能为空」），落回平台数据目录。
func TestResolveRejectsEmptyRootAfterTrim(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	file, err := Load(writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: [a/b]\nroot: \"   \"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := file.DataRoot(), filepath.Join(data, "Cosmic-Developers-Union", "assistant"); got != want {
		t.Errorf("DataRoot = %q, want %q", got, want)
	}
}

// TestLoadRejectsUnreadablePath 覆盖读取失败：路径不存在与路径是目录都必须
// 在 Load 阶段报「读取 run 配置」错误，而不是留给下游静默失败。
func TestLoadRejectsUnreadablePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := Load(filepath.Join(t.TempDir(), FileName)); err == nil ||
		!strings.Contains(err.Error(), "读取 run 配置") {
		t.Errorf("err = %v, want 读取 run 配置 报错", err)
	}
	if _, err := Load(t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "读取 run 配置") {
		t.Errorf("err = %v, want 读取 run 配置 报错", err)
	}
}

// TestLoadRejectsMalformedYAML 覆盖 YAML 本身解析失败（缩进错乱）与类型
// 不匹配：报「解析 run 配置」错误并带上路径。
func TestLoadRejectsMalformedYAML(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := writeTemp(t, FileName, "monitor:\n\trepos: [a/b]\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "解析 run 配置") {
		t.Errorf("err = %v, want 解析 run 配置 报错", err)
	}
	path = writeTemp(t, FileName, "\nmonitor:\n  https://gitea.example:\n    repos: not-a-list\n")
	if _, err := Load(path); err == nil {
		t.Error("monitor.repos 类型不匹配应报错")
	}
}

// TestAbsFromBlankAndWhitespace 覆盖 absFrom 的空串归零：空路径与纯空白都
// 返回空串（调用方按「未配置」处理），不拼出 run.yaml 同目录本身。
func TestAbsFromBlankAndWhitespace(t *testing.T) {
	base := t.TempDir()
	tests := map[string]string{
		"空串": "",
		"空白": "   ",
		"换行": "\n\t ",
	}
	for label, input := range tests {
		if got := absFrom(base, input); got != "" {
			t.Errorf("%s：absFrom = %q, want 空串", label, got)
		}
	}
	// 有内容时：相对路径锚定基目录，绝对路径仅 Clean
	if got, want := absFrom(base, "var/data"), filepath.Join(base, "var", "data"); got != want {
		t.Errorf("absFrom = %q, want %q", got, want)
	}
	if got, want := absFrom(base, "/srv//data/"), "/srv/data"; got != want {
		t.Errorf("absFrom = %q, want %q", got, want)
	}
}

// TestResolvePathIgnoresBlankExplicitPath 覆盖显式参数与 ASSISTANT_RUN 只含
// 空白：都按未提供处理，继续走自动发现，而不是当成一个叫 " " 的路径。
func TestResolvePathIgnoresBlankExplicitPath(t *testing.T) {
	t.Setenv("ASSISTANT_RUN", "  ")
	t.Chdir(t.TempDir())
	path, err := ResolvePath("   ", "")
	if err != nil {
		t.Fatalf("ResolvePath error = %v", err)
	}
	if path != "" {
		t.Errorf("ResolvePath = %q, want 空串（空白路径不成立）", path)
	}
}

// TestValidateRejectsEmptyRootField 覆盖 root 未解析（手工构造 File）时的
// 兜底校验：Validate 直接调用也必须拒绝空数据根。
func TestValidateRejectsEmptyRootField(t *testing.T) {
	file := &File{Monitor: map[string]Monitor{"https://gitea.example": {Repos: []string{"a/b"}}}}
	if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("Validate = %v, want root 不能为空", err)
	}
}

// TestValidateRejectsRawDuplicateMonitor 覆盖手工构造的重复站点（未经 YAML
// 解析，因此不受 yaml 语法拦截）：Validate 必须报「重复站点」。
func TestValidateRejectsRawDuplicateMonitor(t *testing.T) {
	file := &File{
		root:    t.TempDir(),
		Monitor: map[string]Monitor{"https://gitea.example": {Repos: []string{"a/b"}}},
	}
	if err := file.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestDefaultPath 覆盖 run.yaml 标准落点：当前工作目录下的 run.yaml
// （DefaultConfigDir 取 cwd）。
func TestDefaultPath(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join(directory, FileName); path != want {
		t.Errorf("DefaultPath = %q, want %q", path, want)
	}
}
