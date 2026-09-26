package repoinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/content"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/skills"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Dir:             dir,
		Tools:           SupportedTools(),
		CodexConfigPath: filepath.Join(t.TempDir(), ".codex", "config.toml"),
		RunSkills:       stubSkillsRunner,
		Log:             func(string, ...any) {},
	}
}

// stubSkillsRunner 模拟 skills CLI：按 agent 落盘/删除技能文件。
func stubSkillsRunner(request SkillsRequest) error {
	paths := map[string]string{
		"claude-code": ".claude/skills/review/SKILL.md",
		"opencode":    ".agents/skills/review/SKILL.md",
		"codex":       ".agents/skills/review/SKILL.md",
	}
	for _, agent := range request.Agents {
		relative, ok := paths[agent]
		if !ok {
			continue
		}
		path := filepath.Join(request.Dir, relative)
		if request.Remove {
			_ = os.Remove(path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(skills.Review), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestInstallCreatesArtifactsAndIsIdempotent(t *testing.T) {
	options := testOptions(t)
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	for _, relative := range append(
		[]string{ManagedSkillPath(), ManagedAgentPath(), ManagedClaudePath(), ManagedReviewPath()},
		ManagedWorkflowPaths()...) {
		path := filepath.Join(options.Dir, relative)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", relative, err)
		}
	}
	if got := readFile(t, filepath.Join(options.Dir, ManagedClaudePath())); got != ClaudeTemplate {
		t.Errorf("CLAUDE.md = %q, want %q", got, ClaudeTemplate)
	}
	// Claude：.mcp.json + settings 放行
	var mcp map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(options.Dir, ".mcp.json"))), &mcp); err != nil {
		t.Fatal(err)
	}
	servers := mcp["mcpServers"].(map[string]any)
	gitea := servers["gitea"].(map[string]any)
	if gitea["command"] != "assistant" {
		t.Errorf("claude mcp gitea = %+v", gitea)
	}
	settings := readFile(t, filepath.Join(options.Dir, ".claude", "settings.json"))
	for _, want := range []string{"enableAllProjectMcpServers", "mcp__gitea", "mcp__gitea__*", "Bash(git diff:*)", "BASH_DEFAULT_TIMEOUT_MS"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings missing %s: %s", want, settings)
		}
	}
	// opencode
	opencode := readFile(t, filepath.Join(options.Dir, "opencode.json"))
	for _, want := range []string{`"gitea"`, `"gitea_*": "allow"`, `"type": "local"`} {
		if !strings.Contains(opencode, want) {
			t.Errorf("opencode.json missing %s: %s", want, opencode)
		}
	}
	// codex 全局配置
	codex := readFile(t, options.CodexConfigPath)
	for _, want := range []string{"[mcp_servers.gitea]", `command = "assistant"`, `approval_policy = "never"`} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex config missing %s: %s", want, codex)
		}
	}
	// workflow：绑定渲染（默认镜像、约定身份 merge）、两个 job、sync 用内置令牌
	workflow := readFile(t, filepath.Join(options.Dir, ManagedWorkflowPaths()[0]))
	for _, want := range []string{
		"image: " + DefaultImage,
		"GITEA_ACCESS_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
		"GITEA_ACCESS_TOKEN: ${{ secrets.MERGE_TOKEN }}",
		"assistant action label-sync --verbose",
		"assistant action automerge --verbose",
		// Gitea/GitHub 只有 types 映射形式才注册事件；列表简写会被静默忽略
		"types: [opened, edited, closed, reopened, labeled, unlabeled]",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow missing %s:\n%s", want, workflow)
		}
	}
	// 身份是约定、merge 是仓库管理员：不再需要 STATE_REVIEWER variable 或
	// 额外的分支保护令牌
	for _, unwanted := range []string{"STATE_REVIEWER", "BRANCH_PROTECTION_TOKEN"} {
		if strings.Contains(workflow, unwanted) {
			t.Errorf("workflow 不应包含 %s:\n%s", unwanted, workflow)
		}
	}
	// AGENTS.md 段落同样绑定；marker 由 install 自动包裹，内容源不写
	agents := readFile(t, filepath.Join(options.Dir, ManagedAgentPath()))
	if !strings.Contains(agents, "@"+ConventionReviewer) || !strings.Contains(agents, "`"+ConventionMerger+"`") {
		t.Errorf("AGENTS.md not bound to conventions:\n%s", agents)
	}
	if !strings.Contains(agents, "<!-- /"+Marker+" -->") {
		t.Errorf("AGENTS.md 缺少 install 包裹的结束 marker:\n%s", agents)
	}
	if strings.Contains(content.AgentsSection, Marker) {
		t.Errorf("content/agents.md 不应包含 marker（install 自动包裹）")
	}
	// 项目评审约定：托管段落绑定身份，段落外用户内容保留
	review := readFile(t, filepath.Join(options.Dir, ManagedReviewPath()))
	for _, want := range []string{Marker, ConventionReviewer, ConventionMerger} {
		if !strings.Contains(review, want) {
			t.Errorf("review.md missing %s:\n%s", want, review)
		}
	}

	// 幂等：第二次运行不改变任何文件
	before := map[string]string{}
	for _, path := range []string{
		filepath.Join(options.Dir, ManagedSkillPath()),
		filepath.Join(options.Dir, ManagedAgentPath()),
		filepath.Join(options.Dir, ManagedClaudePath()),
		filepath.Join(options.Dir, ".mcp.json"),
		filepath.Join(options.Dir, ".claude", "settings.json"),
		filepath.Join(options.Dir, ManagedReviewPath()),
		filepath.Join(options.Dir, "opencode.json"),
		options.CodexConfigPath,
	} {
		before[path] = readFile(t, path)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("second Install() error = %v", err)
	}
	for path, content := range before {
		if readFile(t, path) != content {
			t.Errorf("second install changed %s", path)
		}
	}
}

func TestUninstallKeepsUserContent(t *testing.T) {
	options := testOptions(t)
	// 已有用户自己的 AGENTS.md 与另一台 MCP server
	if err := os.WriteFile(filepath.Join(options.Dir, "AGENTS.md"), []byte("# 用户自己的说明\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(options.Dir, ".mcp.json"),
		[]byte(`{"mcpServers":{"other":{"command":"other"}}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if content := readFile(t, filepath.Join(options.Dir, ManagedAgentPath())); !strings.Contains(content, "用户自己的说明") {
		t.Errorf("install lost user AGENTS.md content: %s", content)
	}
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	// 用户内容保留，生成内容移除
	content := readFile(t, filepath.Join(options.Dir, ManagedAgentPath()))
	if !strings.Contains(content, "用户自己的说明") || strings.Contains(content, Marker) {
		t.Errorf("AGENTS.md after uninstall = %s", content)
	}
	if _, err := os.Stat(filepath.Join(options.Dir, ManagedSkillPath())); !os.IsNotExist(err) {
		t.Error("skill should be removed")
	}
	var mcp map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(options.Dir, ".mcp.json"))), &mcp); err != nil {
		t.Fatal(err)
	}
	servers := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["gitea"]; ok {
		t.Error("gitea MCP entry should be removed")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("other MCP entry should remain")
	}
	if _, err := os.Stat(options.CodexConfigPath); !os.IsNotExist(err) {
		// codex 配置若只剩空内容会被删除
		t.Log("codex config remains (contains user content?)")
	}
}

// 评审约定文件是段落级托管：install 追加托管段落，卸载只摘该段落，用户自有
// 约定（段落外）原样保留。
func TestReviewConventionsPreserveUserContent(t *testing.T) {
	options := testOptions(t)
	options.Tools = []string{"claude"}
	reviewPath := filepath.Join(options.Dir, ManagedReviewPath())
	if err := os.MkdirAll(filepath.Dir(reviewPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewPath, []byte("# 项目自有约定\n禁止修改对外 API\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	content := readFile(t, reviewPath)
	if !strings.Contains(content, "禁止修改对外 API") || !strings.Contains(content, Marker) {
		t.Errorf("install 后 review.md = %s", content)
	}
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	content = readFile(t, reviewPath)
	if !strings.Contains(content, "禁止修改对外 API") || strings.Contains(content, Marker) {
		t.Errorf("uninstall 后 review.md = %s", content)
	}
}

// claude settings 只管理白名单键：install 合并写入 env/权限放行，卸载只摘除
// 仍是我们写入的托管项，用户自有键原样保留（也不触碰用户级配置）。
func TestClaudeSettingsManagedKeys(t *testing.T) {
	options := testOptions(t)
	options.Tools = []string{"claude"}
	settingsPath := filepath.Join(options.Dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	userSettings := `{"env":{"USER_KEY":"keep"},"permissions":{"allow":["Bash(ls:*)"],"deny":["Read(.env)"]}}`
	if err := os.WriteFile(settingsPath, []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(readFile(t, settingsPath)), &document); err != nil {
		t.Fatal(err)
	}
	env := document["env"].(map[string]any)
	if env["USER_KEY"] != "keep" || env["BASH_DEFAULT_TIMEOUT_MS"] != claudecfg.Env["BASH_DEFAULT_TIMEOUT_MS"] {
		t.Errorf("env = %+v", env)
	}
	permissions := document["permissions"].(map[string]any)
	allow := toStringList(permissions["allow"])
	if !contains(allow, "Bash(ls:*)") || !contains(allow, "Bash(git diff:*)") {
		t.Errorf("allow = %v", allow)
	}
	if permissions["deny"] == nil {
		t.Errorf("用户自有 permissions.deny 丢失: %+v", permissions)
	}

	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	// 重新解析到新 map：Unmarshal 不清空旧键，复用 map 会看到过期结果
	var after map[string]any
	if err := json.Unmarshal([]byte(readFile(t, settingsPath)), &after); err != nil {
		t.Fatal(err)
	}
	document = after
	env = document["env"].(map[string]any)
	if env["USER_KEY"] != "keep" || len(env) != 1 {
		t.Errorf("卸载后 env = %+v, want 仅 USER_KEY", env)
	}
	permissions = document["permissions"].(map[string]any)
	allow = toStringList(permissions["allow"])
	if len(allow) != 1 || allow[0] != "Bash(ls:*)" {
		t.Errorf("卸载后 allow = %v", allow)
	}
	if _, ok := document["enableAllProjectMcpServers"]; ok {
		t.Errorf("卸载后不应保留 enableAllProjectMcpServers: %+v", document)
	}
}

func toStringList(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}

// 技能安装/卸载委托给 skills CLI（bunx skills）：install 调 add、uninstall 调
// remove，agent 名按 CLI 约定映射。
func TestInstallDelegatesSkillsToCLI(t *testing.T) {
	options := testOptions(t)
	options.Tools = []string{"claude", "opencode"}
	var requests []SkillsRequest
	options.RunSkills = func(request SkillsRequest) error {
		requests = append(requests, request)
		return stubSkillsRunner(request)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	wantAgents := []string{"claude-code", "opencode"}
	if len(requests) != 1 || requests[0].Remove ||
		!slices.Equal(requests[0].Agents, wantAgents) ||
		requests[0].Source != DefaultSkillsSource {
		t.Fatalf("install skills requests = %+v", requests)
	}
	if _, err := os.Stat(filepath.Join(options.Dir, ".agents/skills/review/SKILL.md")); err != nil {
		t.Errorf("opencode 技能应已安装: %v", err)
	}

	requests = nil
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if len(requests) != 1 || !requests[0].Remove || !slices.Equal(requests[0].Agents, wantAgents) {
		t.Fatalf("uninstall skills requests = %+v", requests)
	}
}

func TestInstallSkipsSkillsWithNoneSource(t *testing.T) {
	options := testOptions(t)
	options.SkillsSource = "none"
	called := false
	options.RunSkills = func(SkillsRequest) error { called = true; return nil }
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if called {
		t.Error("--skills-source none 时不应调用 skills CLI")
	}
}

// 模板按绑定数据渲染：自定义身份与镜像，且不残留模板动作。
func TestInstallRendersBoundTemplates(t *testing.T) {
	options := testOptions(t)
	options.Reviewer = "bot"
	options.Merger = "merger"
	options.Image = "assistant:e2e"
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	workflow := readFile(t, filepath.Join(options.Dir, ManagedWorkflowPaths()[0]))
	for _, want := range []string{"image: assistant:e2e"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow missing %s:\n%s", want, workflow)
		}
	}
	agents := readFile(t, filepath.Join(options.Dir, ManagedAgentPath()))
	// 内容源由维护者编写（可写死约定账号，不强制占位符）：逐字包含 + marker 包裹
	if !strings.Contains(agents, strings.TrimSpace(content.AgentsSection)) {
		t.Errorf("AGENTS.md 未包含内容源正文：\n%s", agents)
	}
	if !strings.Contains(agents, "<!-- "+Marker+" -->") || !strings.Contains(agents, "<!-- /"+Marker+" -->") {
		t.Errorf("AGENTS.md 缺少 marker 包裹：\n%s", agents)
	}
	for _, relative := range append(
		[]string{ManagedSkillPath(), ManagedAgentPath(), ManagedClaudePath(), ManagedReviewPath()},
		ManagedWorkflowPaths()...) {
		content := readFile(t, filepath.Join(options.Dir, relative))
		if strings.Contains(content, "<<") || strings.Contains(content, ">>") {
			t.Errorf("%s still contains template actions", relative)
		}
	}
}

// 旧版独立 automerge.yml：install 迁移清理（仅限带 marker 的），用户手写的不碰。
func TestInstallMigratesLegacyAutomergeWorkflow(t *testing.T) {
	options := testOptions(t)
	path := func() string { return filepath.Join(options.Dir, LegacyWorkflowPaths()[0]) }
	if err := os.MkdirAll(filepath.Dir(path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(), []byte("<!-- managed-by: assistant -->\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if _, err := os.Stat(path()); !os.IsNotExist(err) {
		t.Error("managed legacy automerge workflow should be removed on install")
	}
	// 用户手写的同名文件：install 与 uninstall 都不得触碰
	if err := os.WriteFile(path(), []byte("name: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("second Install() error = %v", err)
	}
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, err := os.Stat(path()); err != nil {
		t.Error("user-owned automerge workflow must survive install/uninstall")
	}
}

// CLAUDE.md 生命周期：install 写入 @AGENTS.md；uninstall 只移除自己写入的内容，
// 用户自有文件（含只摘导入行）不受损。
func TestClaudeMDLifecycle(t *testing.T) {
	options := testOptions(t)
	path := filepath.Join(options.Dir, ManagedClaudePath())
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if got := readFile(t, path); got != ClaudeTemplate {
		t.Fatalf("CLAUDE.md = %q, want %q", got, ClaudeTemplate)
	}
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("CLAUDE.md 应被移除：%v", err)
	}

	// 用户自有（无导入行）：install 跳过、uninstall 保留
	if err := os.WriteFile(path, []byte("# 用户说明\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if got := readFile(t, path); got != "# 用户说明\n" {
		t.Errorf("用户 CLAUDE.md 被改写：%q", got)
	}
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if got := readFile(t, path); got != "# 用户说明\n" {
		t.Errorf("用户 CLAUDE.md 被删除/改写：%q", got)
	}

	// 用户内容 + 导入行：uninstall 只摘导入行
	if err := os.WriteFile(path, []byte("# 用户说明\n@AGENTS.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if got := readFile(t, path); got != "# 用户说明\n" {
		t.Errorf("应只摘掉导入行：%q", got)
	}
}

// InstallWorkflows 只动 workflow：它是 `assistant install gitea-actions` 的
// 实现路径，不能顺带重写 AGENTS.md / 技能等其它产物（否则一条只读命令的
// 语义就变了）。同时要清理旧版独立 automerge workflow 的 marker 文件。
func TestInstallWorkflowsOnlyTouchesWorkflows(t *testing.T) {
	options := testOptions(t)
	// 预置一个用户自有文件与一个旧版 workflow，验证前者不动、后者被清
	agentPath := filepath.Join(options.Dir, ManagedAgentPath())
	userContent := "# 手写的协作约定\n"
	if err := os.WriteFile(agentPath, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := LegacyWorkflowPaths()[0]
	legacyPath := filepath.Join(options.Dir, legacy)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(Marker+"\n旧版 workflow\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := InstallWorkflows(options); err != nil {
		t.Fatalf("InstallWorkflows() error = %v", err)
	}

	for _, relative := range ManagedWorkflowPaths() {
		content := readFile(t, filepath.Join(options.Dir, relative))
		if !strings.Contains(content, Marker) {
			t.Errorf("%s 缺少 marker", relative)
		}
	}
	if got := readFile(t, agentPath); got != userContent {
		t.Errorf("InstallWorkflows 改动了 AGENTS.md：%q", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("旧版 workflow %s 未被清理（err=%v）", legacy, err)
	}
	// 幂等：再跑一次不应报错
	if err := InstallWorkflows(options); err != nil {
		t.Errorf("第二次 InstallWorkflows() error = %v", err)
	}
}

// InstallWorkflows 的入参校验与 Install 同口径：非法的 --tools 值必须被拒
// （否则会写出一份半成品产物）。
func TestInstallWorkflowsValidatesOptions(t *testing.T) {
	options := testOptions(t)
	options.Tools = []string{"unsupported-tool"}
	if err := InstallWorkflows(options); err == nil {
		t.Error("error = nil, want 不支持的 tools 报错")
	}
	// dry-run 下不得落盘
	dryRun := testOptions(t)
	dryRun.DryRun = true
	if err := InstallWorkflows(dryRun); err != nil {
		t.Fatalf("dry-run InstallWorkflows() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dryRun.Dir, ManagedWorkflowPaths()[0])); !os.IsNotExist(err) {
		t.Error("dry-run 不应写文件")
	}
}

// normalize：Dir 补缺省并转绝对路径；空 Tools 补全支持列表；非法工具报错并
// 列出可选项（错误信息要能直接指导用户改命令行）。
func TestNormalizeOptions(t *testing.T) {
	t.Run("补全缺省并转绝对路径", func(t *testing.T) {
		options := Options{Dir: ".", Log: func(string, ...any) {}}
		if err := options.normalize(); err != nil {
			t.Fatalf("normalize() error = %v", err)
		}
		if !filepath.IsAbs(options.Dir) {
			t.Errorf("Dir = %q, want 绝对路径", options.Dir)
		}
		if !slices.Equal(options.Tools, SupportedTools()) {
			t.Errorf("Tools = %v, want 补全为支持列表", options.Tools)
		}
	})

	t.Run("空 Dir 落到当前目录", func(t *testing.T) {
		options := Options{Log: func(string, ...any) {}}
		if err := options.normalize(); err != nil {
			t.Fatalf("normalize() error = %v", err)
		}
		if !filepath.IsAbs(options.Dir) {
			t.Errorf("Dir = %q, want 绝对路径", options.Dir)
		}
	})

	t.Run("非法工具报错并列出可选项", func(t *testing.T) {
		options := Options{Dir: t.TempDir(), Tools: []string{"claude", "emacs"}, Log: func(string, ...any) {}}
		err := options.normalize()
		if err == nil {
			t.Fatal("error = nil, want 不支持的 --tools 值")
		}
		if !strings.Contains(err.Error(), "emacs") {
			t.Errorf("error = %v, want 点名非法值", err)
		}
		for _, supported := range SupportedTools() {
			if !strings.Contains(err.Error(), supported) {
				t.Errorf("error = %v, want 列出可选项 %s", err, supported)
			}
		}
	})

	t.Run("Tools 被复制而非共享", func(t *testing.T) {
		options := Options{Dir: t.TempDir(), Tools: []string{"claude"}, Log: func(string, ...any) {}}
		if err := options.normalize(); err != nil {
			t.Fatal(err)
		}
		options.Tools[0] = "mutated"
		if SupportedTools()[0] == "mutated" {
			t.Error("normalize 与内置列表共享了底层数组")
		}
	})
}

// write/remove 的 dry-run 分支：dry-run 必须零副作用且能在日志里看到意图。
func TestWriteAndRemoveDryRun(t *testing.T) {
	var logs []string
	options := Options{Dir: t.TempDir(), DryRun: true, Log: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}}
	path := filepath.Join(options.Dir, "sub", "file.txt")

	if err := options.write(path, "内容"); err != nil {
		t.Fatalf("write() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("dry-run 不应落盘")
	}

	// remove 对不存在的文件在 dry-run 下也不应报错
	if err := options.remove(filepath.Join(options.Dir, "not-there.txt")); err != nil {
		t.Fatalf("remove() error = %v, want dry-run 不报错", err)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "dry-run") {
		t.Errorf("日志未标明 dry-run：%q", joined)
	}
	if !strings.Contains(joined, filepath.Join("sub", "file.txt")) {
		t.Errorf("日志未给出相对路径：%q", joined)
	}
}

// remove 对不存在的文件静默成功（幂等卸载），对真实文件则删除。
func TestRemoveIsIdempotent(t *testing.T) {
	options := testOptions(t)
	path := filepath.Join(options.Dir, "gone.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := options.remove(path); err != nil {
		t.Fatalf("remove() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("文件未被删除")
	}
	// 再删一次：不存在不算错误，否则重跑 install/uninstall 会中断
	if err := options.remove(path); err != nil {
		t.Errorf("第二次 remove() error = %v, want nil（幂等）", err)
	}
}
