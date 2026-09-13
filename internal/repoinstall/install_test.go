package repoinstall

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"assistant/content"
	"assistant/skills"
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
	for _, relative := range append([]string{ManagedSkillPath(), ManagedAgentPath(), ManagedClaudePath()}, ManagedWorkflowPaths()...) {
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
	for _, want := range []string{"enableAllProjectMcpServers", "mcp__gitea", "mcp__gitea__*"} {
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
		"assistant sync --verbose",
		"assistant automerge --verbose",
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

	// 幂等：第二次运行不改变任何文件
	before := map[string]string{}
	for _, path := range []string{
		filepath.Join(options.Dir, ManagedSkillPath()),
		filepath.Join(options.Dir, ManagedAgentPath()),
		filepath.Join(options.Dir, ManagedClaudePath()),
		filepath.Join(options.Dir, ".mcp.json"),
		filepath.Join(options.Dir, ".claude", "settings.json"),
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

func TestDetectTokenOrder(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")
	if token, source, ok := DetectToken(os.Getenv); !ok || token != "env-token" || source != "GITEA_ACCESS_TOKEN" {
		t.Errorf("DetectToken() = %q %q %v, want env token", token, source, ok)
	}
	t.Setenv("GITEA_ACCESS_TOKEN", "")
	ours := filepath.Join(configDir, "Cosmic-Developers-Union", "assistant", "token")
	if err := os.MkdirAll(filepath.Dir(ours), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ours, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, source, ok := DetectToken(os.Getenv); !ok || token != "file-token" || source != ours {
		t.Errorf("DetectToken() = %q %q %v, want file token", token, source, ok)
	}
}

func TestResolveMCPFromGitRemote(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	if output, err := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"http://gitea.example.com:3000/owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v %s", err, output)
	}
	t.Setenv("GITEA_ACCESS_TOKEN", "dev-token")
	spec, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   dir,
		Probe: func(host string) bool { return host == "http://gitea.example.com:3000" },
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if spec.Host != "http://gitea.example.com:3000" || spec.HostSource != "remote origin" {
		t.Errorf("host = %q (%s)", spec.Host, spec.HostSource)
	}
	if spec.Token != "dev-token" || spec.TokenSource != "GITEA_ACCESS_TOKEN" {
		t.Errorf("token source = %q", spec.TokenSource)
	}
}

// 多个 remote：origin 指向 GitHub 时探测并选中 Gitea remote。
func TestResolveMCPPicksGiteaRemote(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	for name, url := range map[string]string{
		"origin": "git@github.com:owner/repo.git",
		"gitea":  "http://gitea.example.com:3000/owner/repo.git",
	} {
		if output, err := exec.Command("git", "-C", dir, "remote", "add", name, url).CombinedOutput(); err != nil {
			t.Fatalf("git remote add %s: %v %s", name, err, output)
		}
	}
	t.Setenv("GITEA_ACCESS_TOKEN", "dev-token")
	spec, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   dir,
		Probe: func(host string) bool { return host == "http://gitea.example.com:3000" },
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if spec.Host != "http://gitea.example.com:3000" || spec.HostSource != "remote gitea" {
		t.Errorf("host = %q (%s), want gitea remote", spec.Host, spec.HostSource)
	}
}

func TestMCPCommandDefaults(t *testing.T) {
	command, args := mcpCommand(func(string) string { return "" })
	if command != "go" || args[0] != "run" || !strings.Contains(args[1], "gitea-mcp") {
		t.Errorf("mcpCommand() = %q %v", command, args)
	}
	command, args = mcpCommand(func(name string) string {
		if name == "GITEA_MCP_BIN" {
			return "/usr/local/bin/gitea-mcp"
		}
		return ""
	})
	if command != "/usr/local/bin/gitea-mcp" || args[0] != "-t" {
		t.Errorf("mcpCommand(bin) = %q %v", command, args)
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
	for _, relative := range append([]string{ManagedSkillPath(), ManagedAgentPath(), ManagedClaudePath()}, ManagedWorkflowPaths()...) {
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
