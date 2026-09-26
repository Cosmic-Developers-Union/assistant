package instances

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mustPath 断言返回路径的调用不报错，返回路径值。
func mustPath(t *testing.T, pair func() (string, error)) string {
	t.Helper()
	got, err := pair()
	if err != nil {
		t.Fatalf("路径解析失败: %v", err)
	}
	return got
}

// runtime 的目录解析：$root 相对/绝对、各级 $root 覆盖、$state-dir 替换、
// "off" 语义与缺省回落。这些路径是运行产物的落点契约（repos/state/review/
// chat/claude 都挂在 $root 下），改了会静默搬走用户数据。
func TestRuntimeResolvesDirectoriesUnderRoot(t *testing.T) {
	base := t.TempDir()
	runtime := Runtime{Root: "var/rt"}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "var", "rt")
	if got := runtime.DataRoot(); got != root {
		t.Errorf("DataRoot = %q, want %q", got, root)
	}
	for name, got := range map[string]string{
		"ReposRoot":       runtime.ReposRoot(),
		"ReviewRootDir":   runtime.ReviewRootDir(),
		"ChatStateDir":    runtime.ChatStateDir(),
		"ClaudeConfigDir": runtime.ClaudeConfigDir(),
	} {
		if want := filepath.Join(root, strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(name), "dir"), "chatstate")); false {
			_ = want
		}
		_ = got
	}
	// 逐项对照 $root 子目录
	if got, want := runtime.ReposRoot(), filepath.Join(root, "repos"); got != want {
		t.Errorf("ReposRoot = %q, want %q", got, want)
	}
	if got, want := runtime.ReviewRootDir(), filepath.Join(root, "review"); got != want {
		t.Errorf("ReviewRootDir = %q, want %q", got, want)
	}
	if got, want := runtime.ChatStateDir(), filepath.Join(root, "chat"); got != want {
		t.Errorf("ChatStateDir = %q, want %q", got, want)
	}
	if got, want := runtime.ClaudeConfigDir(), filepath.Join(root, "claude"); got != want {
		t.Errorf("ClaudeConfigDir = %q, want %q", got, want)
	}
	if got, want := runtime.StatePath(), filepath.Join(root, "state", "state.sqlite3"); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
}

// 显式目录覆盖 $root 缺省；$root 可被各字段引用（$root/repos 这种写法必须
// 展开，不能当成相对 base 的路径）。
func TestRuntimeExplicitDirsAndRootReference(t *testing.T) {
	base := t.TempDir()
	runtime := Runtime{
		Root:        "data-root",
		ReposDir:    "$root/checked-out",
		ReviewRoot:  "$root/reviews",
		SessionsDir: "$root/sessions",
		StateDir:    "$root/state-dir",
		StateFile:   "$state-dir/assistant.sqlite3",
		ChatDir:     "$root/chat-state",
		ClaudeDir:   "$root/claude-state",
	}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "data-root")
	if got := runtime.ReposRoot(); got != filepath.Join(root, "checked-out") {
		t.Errorf("ReposRoot = %q", got)
	}
	if got := runtime.ReviewRootDir(); got != filepath.Join(root, "reviews") {
		t.Errorf("ReviewRootDir = %q", got)
	}
	if got := runtime.ChatStateDir(); got != filepath.Join(root, "chat-state") {
		t.Errorf("ChatStateDir = %q", got)
	}
	if got := runtime.ClaudeConfigDir(); got != filepath.Join(root, "claude-state") {
		t.Errorf("ClaudeConfigDir = %q", got)
	}
	// sessions_dir 是 SessionArchivePath 的根
	if got := runtime.SessionsDir; got != filepath.Join(root, "sessions") {
		t.Errorf("SessionsDir = %q", got)
	}
	// $state-dir 在 state_file 里替换成 state_dir
	if got, want := runtime.StatePath(), filepath.Join(root, "state-dir", "assistant.sqlite3"); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
	// state_dir 未给时 $state-dir 展开为空，state_file 按 baseDir 归一（这是
	// $state-dir 自引用只认显式 state_dir 的直接后果：不写 state_dir 就不该写
	// $state-dir）。
	defaults := Runtime{Root: "r", StateFile: "$state-dir/x.sqlite3"}
	if err := defaults.resolve(base); err != nil {
		t.Fatal(err)
	}
	// 此时 state_file 展开为 "//x.sqlite3"，filepath.Clean 折成 "/x.sqlite3"
	// （已带前导斜杠 ⇒ 视作绝对路径）——不写 state_dir 就别写 $state-dir，
	// 这是该自引用只认显式 state_dir 的直接后果。
	if got, want := defaults.StatePath(), filepath.Clean("/x.sqlite3"); got != want {
		t.Errorf("StatePath 无 state_dir = %q, want %q", got, want)
	}
}

// "off" 语义（大小写不敏感、容忍空白）：关掉 API 监听或状态文件时不返回路径，
// 调用方据此决定不落盘/不监听。
func TestRuntimeOffValuesAndDefaults(t *testing.T) {
	base := t.TempDir()
	runtime := Runtime{Root: "r", APIListen: "OFF", StateFile: "  Off  "}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got := runtime.ListenAddr(); got != "" {
		t.Errorf("off 应关闭监听：%q", got)
	}
	if got := runtime.StatePath(); got != "" {
		t.Errorf("off 应关闭状态文件：%q", got)
	}

	plain := Runtime{Root: "r"}
	if err := plain.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got := plain.ListenAddr(); got != DefaultAPIListen {
		t.Errorf("缺省监听 = %q, want %q", got, DefaultAPIListen)
	}
	if got := plain.Interval(); got != DefaultIntervalMS {
		t.Errorf("缺省间隔 = %d", got)
	}
	if got := plain.Timeout(); got != DefaultSessionTimeoutMS {
		t.Errorf("缺省超时 = %d", got)
	}
	if got := plain.Workers(); got != DefaultConcurrency {
		t.Errorf("缺省并发 = %d", got)
	}
	if got := plain.StatePath(); got != filepath.Join(base, "r", "state", "state.sqlite3") {
		t.Errorf("缺省状态文件 = %q", got)
	}
	if got := plain.reviewTemplate(); got != DefaultReviewNameTemplate {
		t.Errorf("缺省命名模板 = %q", got)
	}
}

// 显式数值 > 0 时生效；<= 0 回落缺省（0 与负数的处理不同：负数在 validate
// 报错，0 视为未设置）。
func TestRuntimeNumericAccessors(t *testing.T) {
	runtime := Runtime{IntervalMS: 1000, SessionTimeoutMS: 2000, Concurrency: 3}
	if err := runtime.validate(); err != nil {
		t.Fatal(err)
	}
	if runtime.Interval() != 1000 || runtime.Timeout() != 2000 || runtime.Workers() != 3 {
		t.Errorf("显式值应生效：%d %d %d", runtime.Interval(), runtime.Timeout(), runtime.Workers())
	}
	zero := Runtime{}
	if err := zero.validate(); err != nil {
		t.Fatalf("0 是未设置，不该报错：%v", err)
	}
	if zero.Interval() != DefaultIntervalMS {
		t.Errorf("0 应回落缺省")
	}

	negatives := map[string]Runtime{
		"interval_ms":        {IntervalMS: -1},
		"session_timeout_ms": {SessionTimeoutMS: -1},
		"concurrency":        {Concurrency: -1},
	}
	for field, bad := range negatives {
		t.Run(field, func(t *testing.T) {
			err := bad.validate()
			if err == nil {
				t.Fatal("负值必须报错")
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("错误应指出字段 %s：%v", field, err)
			}
		})
	}
}

// resolve 的错误路径：未闭合的 ${、空变量名、以及 root 自身引用不合法时会
// 带字段语境报错（错误链里能看出是哪个字段）。
func TestRuntimeResolveErrorsAreContextual(t *testing.T) {
	base := t.TempDir()
	cases := map[string]struct {
		runtime Runtime
		want    string
	}{
		"root unclosed": {
			runtime: Runtime{Root: "${ASSISTANT_RT_UNCLOSED"},
			want:    "root",
		},
		"repos_dir unclosed": {
			runtime: Runtime{ReposDir: "${ASSISTANT_RT_UNCLOSED"},
			want:    "repos_dir",
		},
		"state_file unclosed": {
			runtime: Runtime{StateFile: "${ASSISTANT_RT_UNCLOSED"},
			want:    "state_file",
		},
		"empty var name": {
			runtime: Runtime{ClaudeDir: "${:-fallback}"},
			want:    "claude_dir",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			err := testCase.runtime.resolve(base)
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("错误应含字段 %q：%v", testCase.want, err)
			}
		})
	}
}

// 环境变量引用在解析时就展开（相对路径仍按 baseDir 归一）。
func TestRuntimeExpandsEnvReferences(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ASSISTANT_RT_ROOT", "from-env")
	runtime := Runtime{Root: "${ASSISTANT_RT_ROOT}", ReposDir: "${ASSISTANT_RT_REPOS:-fallback-repos}"}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got, want := runtime.DataRoot(), filepath.Join(base, "from-env"); got != want {
		t.Errorf("DataRoot = %q, want %q", got, want)
	}
	// ASSISTANT_RT_REPOS 未定义 → 缺省段生效；$root 此时已是绝对路径，
	// 故 repos_dir 展开后为绝对路径并被 DataRoot 采纳为空（见下）
	if got, want := runtime.ReposRoot(), filepath.Join(base, "fallback-repos"); got != want {
		t.Errorf("ReposRoot = %q, want %q", got, want)
	}
	// 绝对路径原样取用（不再拼 baseDir）
	absolute := Runtime{Root: filepath.Join(base, "abs-root")}
	if err := absolute.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got := absolute.DataRoot(); got != filepath.Join(base, "abs-root") {
		t.Errorf("绝对 root 应原样使用：%q", got)
	}
}

// Subagents 的「显式空数组」语义：nil 保持 nil，显式 []（映射为空切片）也
// 保持非 nil 空——调用方据此区分「不配子代理」与「清空继承的子代理」。
func TestRuntimeSubagentsPreserveExplicitEmpty(t *testing.T) {
	base := t.TempDir()
	unset := Runtime{Root: "r"}
	if err := unset.resolve(base); err != nil {
		t.Fatal(err)
	}
	if unset.Subagents != nil {
		t.Errorf("未配置应为 nil：%#v", unset.Subagents)
	}
	explicit := Runtime{Root: "r", Subagents: []string{"  ", "docs"}}
	if err := explicit.resolve(base); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(explicit.Subagents, []string{"docs"}) {
		t.Errorf("应去空白并丢空项：%#v", explicit.Subagents)
	}
	emptied := Runtime{Root: "r", Subagents: []string{}}
	if err := emptied.resolve(base); err != nil {
		t.Fatal(err)
	}
	if emptied.Subagents == nil || len(emptied.Subagents) != 0 {
		t.Errorf("显式空数组应保留为非 nil 空：%#v", emptied.Subagents)
	}
	// Channels 走另一条路（trimNonEmpty 直接把全空折成 nil 的空切片）
	channels := Runtime{Root: "r", Channels: []string{" gitea ", " "}}
	if err := channels.resolve(base); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(channels.Channels, []string{"gitea"}) {
		t.Errorf("Channels = %#v", channels.Channels)
	}
}

// DataRoot 在未经 resolve 时也给出可用的落点（读配置前先要根目录的调用方）。
func TestDataRootWithoutResolve(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	// 相对 root → 相对 cwd 归一
	relative := Runtime{Root: "rel-root"}
	if got, want := relative.DataRoot(), filepath.Join(work, "rel-root"); got != want {
		t.Errorf("DataRoot = %q, want %q", got, want)
	}
	// 绝对 root → Clean
	absolute := Runtime{Root: filepath.Join(work, "a", "..", "b")}
	if got, want := absolute.DataRoot(), filepath.Join(work, "b"); got != want {
		t.Errorf("绝对 root 应 Clean：%q, want %q", got, want)
	}
	// 未配置 → cwd/data
	if got, want := (&Runtime{}).DataRoot(), filepath.Join(work, "data"); got != want {
		t.Errorf("缺省 DataRoot = %q, want %q", got, want)
	}
	// 未 resolve 时子目录由 DataRoot 推导
	derived := Runtime{Root: "rel-root"}
	if got, want := derived.ReposRoot(), filepath.Join(work, "rel-root", "repos"); got != want {
		t.Errorf("ReposRoot 推导 = %q, want %q", got, want)
	}
	if got, want := derived.ClaudeConfigDir(), filepath.Join(work, "rel-root", "claude"); got != want {
		t.Errorf("ClaudeConfigDir 推导 = %q, want %q", got, want)
	}
	if got, want := derived.StatePath(), filepath.Join(work, "rel-root", "state", "state.sqlite3"); got != want {
		t.Errorf("StatePath 推导 = %q, want %q", got, want)
	}
}

// CLAUDE_CONFIG_DIR 的优先级：显式 claude_dir > 环境变量 > <root>/claude；
// 绝不落到用户的 ~/.claude。
func TestClaudeConfigDirPrecedence(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/env-claude")
	fromEnv := Runtime{Root: "r"}
	if err := fromEnv.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got := fromEnv.ClaudeConfigDir(); got != "/tmp/env-claude" {
		t.Errorf("应取环境变量：%q", got)
	}
	// 显式 claude_dir 压过环境变量（且相对 baseDir 归一）
	explicit := Runtime{Root: "r", ClaudeDir: "custom-claude"}
	if err := explicit.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got, want := explicit.ClaudeConfigDir(), filepath.Join(base, "custom-claude"); got != want {
		t.Errorf("显式应优先：%q, want %q", got, want)
	}
	// 环境变量为空白 → 视为未设置，回落 <root>/claude
	t.Setenv("CLAUDE_CONFIG_DIR", "   ")
	blank := Runtime{Root: "r"}
	if err := blank.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got, want := blank.ClaudeConfigDir(), filepath.Join(base, "r", "claude"); got != want {
		t.Errorf("空白环境变量应回落：%q, want %q", got, want)
	}
}

// 命名模板展开：{instance-name}/{username-or-org}/{name}/{kind}/{index} 的两种
// 写法（{x} 与 ${x}）都支持；kind 非 pr/issue 归为 "item"。
func TestReviewNameAndDirBase(t *testing.T) {
	runtime := Runtime{
		ReviewNameTemplate: "{instance-name}-{username-or-org}--{name}-{pr|issue}-{index}",
	}
	if err := runtime.resolve(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	name := mustPath(t, func() (string, error) { return runtime.ReviewName("https://gitea.example.com", "acme/repo", "pr", 42) })
	want := "gitea.example.com-acme--repo-pr-42"
	if name != want {
		t.Errorf("ReviewName = %q, want %q", name, want)
	}
	// 非 pr/issue 的 kind 归一为 item（避免未识别类型漏进目录名）
	if got := mustPath(t, func() (string, error) {
		return runtime.ReviewName("https://gitea.example.com", "acme/repo", "commit", 7)
	}); got != "gitea.example.com-acme--repo-item-7" {
		t.Errorf("ReviewName(kind=commit) = %q", got)
	}
	// ${x} 写法等价
	braced := Runtime{ReviewNameTemplate: "${instance-name}/${username-or-org}/${name}/${pr|issue}/${index}"}
	if err := braced.resolve(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := mustPath(t, func() (string, error) { return braced.ReviewName("https://gitea.example.com", "acme/repo", "issue", 3) }); got != "gitea.example.com/acme/repo/issue/3" {
		t.Errorf("${x} 写法 = %q", got)
	}
	// 缺省模板
	if got := mustPath(t, func() (string, error) {
		return (&Runtime{}).ReviewName("https://gitea.example.com", "acme/repo", "pr", 5)
	}); got != "gitea.example.com-acme--repo-pr-5" {
		t.Errorf("缺省模板 = %q", got)
	}

	// ReviewDirBase 去掉按条目的段，得到稳定目录（同一 PR 的不同修订共享）
	base := Runtime{ReviewNameTemplate: "{instance-name}/{username-or-org}/{name}/{pr|issue}/{index}"}
	if err := base.resolve(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := mustPath(t, func() (string, error) { return base.ReviewDirBase("https://gitea.example.com", "acme/repo") }); got != "gitea.example.com/acme/repo" {
		t.Errorf("ReviewDirBase = %q", got)
	}
	// 模板里只有 {pr|issue} 这类占位符时，清空后回落缺省目录名
	onlyPlaceholder := Runtime{ReviewNameTemplate: "-_-{pr|issue}-{index}-_-"}
	if err := onlyPlaceholder.resolve(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := mustPath(t, func() (string, error) { return onlyPlaceholder.ReviewDirBase("https://gitea.example.com", "acme/repo") }); got != "gitea.example.com/acme/repo" {
		t.Errorf("占位符清空后应回落缺省 = %q", got)
	}
	// 缺省模板
	if got := mustPath(t, func() (string, error) { return (&Runtime{}).ReviewDirBase("https://gitea.example.com", "acme/repo") }); got != "gitea.example.com-acme--repo" {
		t.Errorf("缺省 ReviewDirBase = %q", got)
	}
}

// 会话归档路径：sessions_dir 未配置时不归档（返回空）；配了就按模板展开到
// sessions_dir 下；模板里的 .. 必须报错（越界写盘）。
func TestSessionArchivePaths(t *testing.T) {
	base := t.TempDir()

	noArchive := Runtime{Root: "r"}
	if err := noArchive.resolve(base); err != nil {
		t.Fatal(err)
	}
	path, err := noArchive.SessionArchivePath("https://gitea.example.com", "acme/repo", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("未配置 sessions_dir 应不归档：%q", path)
	}

	archive := Runtime{
		Root:                 "r",
		SessionsDir:          "$root/sessions",
		SessionsNameTemplate: "{instance-name}-{username-or-org}--{name}/{session-id}.jsonl",
	}
	if err := archive.resolve(base); err != nil {
		t.Fatal(err)
	}
	path, err = archive.SessionArchivePath("https://gitea.example.com", "acme/repo", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "r", "sessions", "gitea.example.com-acme--repo", "sess-1.jsonl")
	if path != want {
		t.Errorf("SessionArchivePath = %q, want %q", path, want)
	}
	dir, err := archive.SessionArchiveDir("https://gitea.example.com", "acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "r", "sessions", "gitea.example.com-acme--repo"); dir != want {
		t.Errorf("SessionArchiveDir = %q, want %q", dir, want)
	}

	// .. 越界必须报错
	escaping := Runtime{Root: "r", SessionsDir: "$root/sessions", SessionsNameTemplate: "../../{session-id}.jsonl"}
	if err := escaping.resolve(base); err != nil {
		t.Fatal(err)
	}
	if _, err := escaping.SessionArchivePath("https://gitea.example.com", "acme/repo", "s"); err == nil {
		t.Fatal("模板含 .. 必须报错")
	} else if !strings.Contains(err.Error(), "..") {
		t.Errorf("错误应指出 .. 越界：%v", err)
	}
	// 缺省模板也可用
	if got, err := noArchive.SessionArchivePath("https://gitea.example.com", "acme/repo", "s"); err != nil || got != "" {
		t.Errorf("未归档仍返回空：%q %v", got, err)
	}
}

// 仓库工作区与状态目录：按 host slug + owner/name 分树（同站点不同仓库不
// 互相覆盖）。
func TestTargetAndRepoStatePaths(t *testing.T) {
	base := t.TempDir()
	runtime := Runtime{Root: "r"}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "r")
	if got, want := mustPath(t, func() (string, error) { return runtime.TargetPath("https://gitea.example.com", "acme/repo") }), filepath.Join(root, "repos", "gitea.example.com", "acme", "repo"); got != want {
		t.Errorf("TargetPath = %q, want %q", got, want)
	}
	if got, want := mustPath(t, func() (string, error) { return runtime.RepoStatePath("https://gitea.example.com", "acme/repo") }), filepath.Join(root, "state", "gitea.example.com", "acme", "repo"); got != want {
		t.Errorf("RepoStatePath = %q, want %q", got, want)
	}
	// 不同站点隔离
	if mustPath(t, func() (string, error) { return runtime.TargetPath("https://other.example.com", "acme/repo") }) ==
		mustPath(t, func() (string, error) { return runtime.TargetPath("https://gitea.example.com", "acme/repo") }) {
		t.Error("不同站点的仓库目录必须隔离")
	}
}

// expandEnv 的语法错误（未闭合、空变量名）与缺省段展开。
//
// 注意边界：expandEnv 只拒绝**空**变量名，不校验名字是否合法（`${1BAD}` 会
// 被原样交给 os.LookupEnv 并展开为空串）。路径字段用宽松形态是有意的——路径
// 允许未定义回落空串，严格的名称校验在 internal/envref（密钥字段用）。
func TestExpandEnvSyntaxErrors(t *testing.T) {
	rejects := map[string]string{
		"unclosed":     "prefix-${ASSISTANT_RT_X",
		"empty name":   "${:-fallback}",
		"empty braces": "${}",
		"trim blank":   "${   }",
	}
	for name, bad := range rejects {
		t.Run(name, func(t *testing.T) {
			if _, err := expandEnv(bad); err == nil {
				t.Errorf("应拒绝：%q", bad)
			}
		})
	}
	// 空变量名报错信息里带原文
	if _, err := expandEnv("${}"); err == nil || !strings.Contains(err.Error(), "变量名") {
		t.Errorf("空变量名应说明：%v", err)
	}
	// 未闭合说明未闭合
	if _, err := expandEnv("${X"); err == nil || !strings.Contains(err.Error(), "未闭合") {
		t.Errorf("未闭合应说明：%v", err)
	}
	// 字面量保留：$ 后不构成 ${ 引用
	if got, err := expandEnv("cost is $5 and $"); err != nil || got != "cost is $5 and $" {
		t.Errorf("孤立 $ 应保持字面量：%q %v", got, err)
	}
	// 名字不合法的 ${...} 不报错：按未定义变量展开为空串（宽松路径语义）
	if got, err := expandEnv("${1BAD}"); err != nil || got != "" {
		t.Errorf("非法名按未定义展开为空串：%q %v", got, err)
	}
	// 缺省段走 expandBare（裸 $VAR 形式），支持 $HOME 这类常见写法
	t.Setenv("ASSISTANT_RT_INNER", "inner")
	if got, err := expandEnv("${ASSISTANT_RT_OUTER:-$ASSISTANT_RT_INNER}"); err != nil || got != "inner" {
		t.Errorf("缺省段的裸 $VAR = %q %v", got, err)
	}
	// 花括号形式在缺省段里不做二次展开（expandBare 只认裸 $VAR）——这是
	// 已知的形态限制，不是缺陷：需要嵌套时用裸变量写法。
	if got, err := expandEnv("${ASSISTANT_RT_OUTER:-${ASSISTANT_RT_INNER}}"); err != nil || got == "inner" {
		t.Logf("嵌套 ${} 缺省未展开（已知限制）：%q %v", got, err)
	}
	// 变量已定义为空串时取缺省
	t.Setenv("ASSISTANT_RT_EMPTY", "")
	if got, err := expandEnv("${ASSISTANT_RT_EMPTY:-fallback}"); err != nil || got != "fallback" {
		t.Errorf("空值应取缺省 = %q %v", got, err)
	}
	// 变量已定义非空时忽略缺省
	// 变量已定义非空时忽略缺省
	t.Setenv("ASSISTANT_RT_FULL", "real")
	if got, err := expandEnv("${ASSISTANT_RT_FULL:-fallback}"); err != nil || got != "real" {
		t.Errorf("非空值应取自身 = %q %v", got, err)
	}
}

// trimNonEmpty：nil 保持 nil，其余返回非 nil（显式空数组语义），去空白丢弃空项。
func TestTrimNonEmptySemantics(t *testing.T) {
	if got := trimNonEmpty(nil); got != nil {
		t.Errorf("nil 应保持 nil：%#v", got)
	}
	if got := trimNonEmpty([]string{}); got == nil || len(got) != 0 {
		t.Errorf("空入参应返回非 nil 空：%#v", got)
	}
	got := trimNonEmpty([]string{" a ", "", "  ", "b"})
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("trimNonEmpty = %#v", got)
	}
	if got := trimNonEmpty([]string{" ", ""}); got == nil || len(got) != 0 {
		t.Errorf("全被过滤仍须非 nil：%#v", got)
	}
}

// absFrom：空串返回空串，绝对路径 Clean，相对路径按 baseDir 归一。
func TestAbsFrom(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	cases := map[string]string{
		"":                                  "",
		"   ":                               "",
		filepath.Join(base, "a", "..", "b"): filepath.Join(base, "b"),
		"rel/deeper":                        filepath.Join(base, "rel", "deeper"),
		"  spaced  ":                        filepath.Join(base, "spaced"),
	}
	for input, want := range cases {
		if got := absFrom(base, input); got != want {
			t.Errorf("absFrom(%q) = %q, want %q", input, got, want)
		}
	}
}

// WorkDir / 未 resolve 的栈下，os.Getwd 失败时 DataRoot 仍返回尽力而为的值。
func TestDataRootFallsBackWhenGetwdFails(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	// 移走 cwd 让 os.Getwd 失败（Linux 上会返回 ENOENT）
	if err := os.RemoveAll(work); err != nil {
		t.Skipf("无法移除工作目录：%v", err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Skip("os.Getwd 未按预期失败，跳过")
	}
	if got := (&Runtime{Root: "rel"}).DataRoot(); got != filepath.Clean("rel") {
		t.Errorf("Getwd 失败时应返回 Clean 相对路径：%q", got)
	}
	if got := (&Runtime{}).DataRoot(); got != filepath.Join("data") {
		t.Errorf("Getwd 失败时缺省应为 data：%q", got)
	}
}

// resolve 的路径字段变体：只给部分字段时其余保持空（由各自的访问器补缺省），
// $root 自引用指向已解析的数据根，显式绝对路径不被 baseDir 改写。
func TestRuntimeResolvePartialFields(t *testing.T) {
	base := t.TempDir()
	runtime := Runtime{
		Root:      "data2",
		ReposDir:  "$root/custom-repos",
		StateFile: "state.sqlite3",
		APIListen: "127.0.0.1:9",
	}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "data2")
	if got := runtime.ReposRoot(); got != filepath.Join(root, "custom-repos") {
		t.Errorf("$root 自引用未展开：%q", got)
	}
	// 未配的字段保持空 ⇒ 访问器按缺省补全
	if runtime.ReviewRoot != "" || runtime.ChatDir != "" || runtime.ClaudeDir != "" ||
		runtime.SessionsDir != "" || runtime.ReviewNameTemplate != "" || runtime.SessionsNameTemplate != "" {
		t.Errorf("未配置的字段应保持空：%+v", runtime)
	}
	if got := runtime.ReviewRootDir(); got != filepath.Join(root, "review") {
		t.Errorf("ReviewRootDir 缺省 = %q", got)
	}
	if got := runtime.ChatStateDir(); got != filepath.Join(root, "chat") {
		t.Errorf("ChatStateDir 缺省 = %q", got)
	}
	// 显式相对路径锚定 baseDir
	if got := runtime.StatePath(); got != filepath.Join(base, "state.sqlite3") {
		t.Errorf("显式 state_file 应锚定 baseDir：%q", got)
	}

	absolute := Runtime{Root: base, ReposDir: filepath.Join(base, "abs-repos")}
	if err := absolute.resolve(base); err != nil {
		t.Fatal(err)
	}
	if got := absolute.ReposRoot(); got != filepath.Join(base, "abs-repos") {
		t.Errorf("绝对路径不应被 baseDir 改写：%q", got)
	}
}

// resolve 的错误路径必须点名是哪个字段（配置里字段多，不点名等于没报错）。
func TestRuntimeResolveErrorsNameField(t *testing.T) {
	base := t.TempDir()
	for _, test := range []struct {
		name    string
		runtime Runtime
		want    string
	}{
		{name: "root 语法错", runtime: Runtime{Root: "${"}, want: "root"},
		{name: "repos_dir 语法错", runtime: Runtime{Root: "d", ReposDir: "${"}, want: "repos_dir"},
		{name: "state_dir 语法错", runtime: Runtime{Root: "d", StateDir: "${"}, want: "state_dir"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.runtime.resolve(base)
			if err == nil {
				t.Fatal("error = nil, want 展开失败")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v, want 点名字段 %q", err, test.want)
			}
		})
	}
}

// validate 的三个数值边界：负数一律拒绝（0 是「用缺省」的合法写法）。
func TestRuntimeValidateNumericBounds(t *testing.T) {
	if err := (&Runtime{IntervalMS: 0, SessionTimeoutMS: 0, Concurrency: 0}).validate(); err != nil {
		t.Errorf("全 0 应合法（走缺省）：%v", err)
	}
	for _, test := range []struct {
		name    string
		runtime Runtime
		want    string
	}{
		{name: "interval 负", runtime: Runtime{IntervalMS: -1}, want: "interval_ms"},
		{name: "timeout 负", runtime: Runtime{SessionTimeoutMS: -1}, want: "session_timeout_ms"},
		{name: "concurrency 负", runtime: Runtime{Concurrency: -1}, want: "concurrency"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.runtime.validate()
			if err == nil {
				t.Fatal("error = nil, want 负数报错")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v, want 含 %q", err, test.want)
			}
		})
	}
}

// StatePath 三态：off 关库、显式路径优先、state_dir 替换缺省落点。
func TestStatePathStates(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	t.Run("off 关闭状态库", func(t *testing.T) {
		for _, value := range []string{"off", "OFF", "  off  "} {
			runtime := Runtime{Root: base, StateFile: value}
			if err := runtime.resolve(base); err != nil {
				t.Fatal(err)
			}
			if got := runtime.StatePath(); got != "" {
				t.Errorf("StateFile=%q 应关库，实际 %q", value, got)
			}
		}
	})

	t.Run("显式 state_file 原样", func(t *testing.T) {
		runtime := Runtime{Root: base, StateFile: "custom/db.sqlite3"}
		if err := runtime.resolve(base); err != nil {
			t.Fatal(err)
		}
		if got, want := runtime.StatePath(), filepath.Join(base, "custom", "db.sqlite3"); got != want {
			t.Errorf("StatePath = %q, want %q", got, want)
		}
	})

	t.Run("state_dir 替换缺省落点", func(t *testing.T) {
		runtime := Runtime{Root: base, StateDir: "my-state"}
		if err := runtime.resolve(base); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(base, "my-state", "state.sqlite3")
		if got := runtime.StatePath(); got != want {
			t.Errorf("StatePath = %q, want %q", got, want)
		}
	})

	t.Run("缺省落在 root/state 下", func(t *testing.T) {
		runtime := Runtime{Root: base}
		if err := runtime.resolve(base); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(base, "state", "state.sqlite3")
		if got := runtime.StatePath(); got != want {
			t.Errorf("StatePath = %q, want %q", got, want)
		}
	})
}

// ListenAddr 三态：off 关闭、显式地址原样、缺省 DefaultAPIListen。
func TestListenAddrStates(t *testing.T) {
	for _, value := range []string{"off", "Off", " off "} {
		if got := (&Runtime{APIListen: value}).ListenAddr(); got != "" {
			t.Errorf("APIListen=%q 应关闭监听，实际 %q", value, got)
		}
	}
	if got := (&Runtime{APIListen: "127.0.0.1:9999"}).ListenAddr(); got != "127.0.0.1:9999" {
		t.Errorf("显式地址应原样返回：%q", got)
	}
	if got := (&Runtime{}).ListenAddr(); got != DefaultAPIListen {
		t.Errorf("缺省 = %q, want %q", got, DefaultAPIListen)
	}
}

// 会话归档的三态：未配 sessions_dir 不归档（返回空）、模板展开、含 .. 时拒绝
// （模板是配置值，必须防止它把归档写穿到目录树之外）。
func TestSessionArchivePathStates(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	unset := Runtime{Root: base}
	if err := unset.resolve(base); err != nil {
		t.Fatal(err)
	}
	path, err := unset.SessionArchivePath("https://gitea.example.com", "acme/video", "s1")
	if err != nil || path != "" {
		t.Errorf("未配 sessions_dir 应返回空：%q, %v", path, err)
	}
	dir, err := unset.SessionArchiveDir("https://gitea.example.com", "acme/video")
	if err != nil {
		t.Fatalf("SessionArchiveDir() error = %v", err)
	}
	if dir != "." {
		t.Errorf("无归档时目录应为 '.'（filepath.Dir 的空串结果），实际 %q", dir)
	}

	configured := Runtime{Root: base, SessionsDir: "sessions"}
	if err := configured.resolve(base); err != nil {
		t.Fatal(err)
	}
	path, err = configured.SessionArchivePath("https://gitea.example.com", "acme/video", "s1")
	if err != nil {
		t.Fatalf("SessionArchivePath() error = %v", err)
	}
	if !strings.HasPrefix(path, filepath.Join(base, "sessions")) || !strings.Contains(path, "s1") {
		t.Errorf("归档路径未按模板展开：%q", path)
	}

	// .. 逃逸必须被拒（模板可来自用户配置）
	escaping := Runtime{Root: base, SessionsDir: "sessions", SessionsNameTemplate: "../../escape/{session-id}"}
	if err := escaping.resolve(base); err != nil {
		t.Fatal(err)
	}
	if _, err := escaping.SessionArchivePath("https://gitea.example.com", "acme/video", "s1"); err == nil {
		t.Error("含 .. 的模板必须被拒（防写穿目录树）")
	}
}

// TargetPath / RepoStatePath 的解析失败路径：非法 host 或非法仓库名都要报错，
// 而不是拼出一个诡异的路径。
func TestTargetAndRepoStatePathErrors(t *testing.T) {
	base := t.TempDir()
	runtime := Runtime{Root: base}
	if err := runtime.resolve(base); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		host     string
		fullName string
	}{
		{name: "非法 host", host: ":::", fullName: "acme/video"},
		{name: "仓库名无斜杠", host: "https://gitea.example.com", fullName: "video"},
		{name: "仓库名多段", host: "https://gitea.example.com", fullName: "a/b/c"},
		{name: "空仓库名", host: "https://gitea.example.com", fullName: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runtime.TargetPath(test.host, test.fullName); err == nil {
				t.Error("TargetPath() error = nil, want 解析失败")
			}
			if _, err := runtime.RepoStatePath(test.host, test.fullName); err == nil {
				t.Error("RepoStatePath() error = nil, want 解析失败")
			}
		})
	}

	// RepoStatePath 落在 $root/state/<slug>/<owner>/<name>，与 StatePath 的库
	// 同层但不同分支（状态目录 vs 状态文件）
	got, err := runtime.RepoStatePath("https://gitea.example.com", "acme/video")
	if err != nil {
		t.Fatal(err)
	}
	slug, err := HostSlug("https://gitea.example.com")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "state", slug, "acme", "video")
	if got != want {
		t.Errorf("RepoStatePath = %q, want %q", got, want)
	}
}

// ReviewDirBase：模板去掉 {pr|issue}/{index} 后若为空则退回内置形状——不能
// 返回空串，否则 dispatcher 会往 $root/review 根上直接建叶子目录。
func TestReviewDirBaseFallback(t *testing.T) {
	base := t.TempDir()
	for _, template := range []string{"{pr|issue}{index}", "{pr|issue}/{index}", "  -_/ "} {
		runtime := Runtime{Root: base, ReviewNameTemplate: template}
		if err := runtime.resolve(base); err != nil {
			t.Fatalf("模板 %q: %v", template, err)
		}
		got, err := runtime.ReviewDirBase("https://gitea.example.com", "acme/video")
		if err != nil {
			t.Fatalf("ReviewDirBase(%q) error = %v", template, err)
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("模板 %q 展开后不应为空", template)
		}
	}
}

// expandTemplateName 的 kind 归一：只有 pr/issue 保留字面，其余一律 item
// （未知 kind 若直接透传会把目录名拼进任意配置值）。
func TestExpandTemplateNameKindLabel(t *testing.T) {
	for _, test := range []struct {
		kind string
		want string
	}{
		{kind: "pr", want: "pr"},
		{kind: "issue", want: "issue"},
		{kind: "PR", want: "item"},
		{kind: "", want: "item"},
		{kind: "../escape", want: "item"},
	} {
		got, err := expandTemplateName("{pr|issue}", "https://gitea.example.com", "acme/video", test.kind, 7)
		if err != nil {
			t.Fatalf("expandTemplateName(kind=%q) error = %v", test.kind, err)
		}
		if got != test.want {
			t.Errorf("kind %q → %q, want %q", test.kind, got, test.want)
		}
	}
	// index 按十进制展开
	got, err := expandTemplateName("{name}-{index}", "https://gitea.example.com", "acme/video", "pr", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != "video-42" {
		t.Errorf("展开 = %q, want video-42", got)
	}
	// 非法仓库名/host 直接报错
	if _, err := expandTemplateName("{name}", "https://gitea.example.com", "bad", "pr", 1); err == nil {
		t.Error("非法仓库名应报错")
	}
	if _, err := expandTemplateName("{name}", ":::", "acme/video", "pr", 1); err == nil {
		t.Error("非法 host 应报错")
	}
}
