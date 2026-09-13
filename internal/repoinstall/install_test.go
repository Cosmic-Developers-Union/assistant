package repoinstall

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Dir:             dir,
		Tools:           SupportedTools(),
		CodexConfigPath: filepath.Join(t.TempDir(), ".codex", "config.toml"),
		Log:             func(string, ...any) {},
	}
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
	for _, relative := range append([]string{ManagedSkillPath(), ManagedAgentPath()}, ManagedWorkflowPaths()...) {
		path := filepath.Join(options.Dir, relative)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", relative, err)
		}
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
		"GITEA_ACCESS_TOKEN: ${{ secrets.STATE_TOKEN }}",
		"GITEA_STATE_REVIEWER: " + ConventionMerger,
		"assistant sync --verbose",
		"assistant automerge --verbose",
		// Gitea/GitHub 只有 types 映射形式才注册事件；列表简写会被静默忽略
		"types: [opened, edited, closed, reopened, labeled, unlabeled]",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow missing %s:\n%s", want, workflow)
		}
	}
	// AGENTS.md 段落同样绑定
	agents := readFile(t, filepath.Join(options.Dir, ManagedAgentPath()))
	if !strings.Contains(agents, "@"+ConventionReviewer) || !strings.Contains(agents, "`"+ConventionMerger+"`") {
		t.Errorf("AGENTS.md not bound to conventions:\n%s", agents)
	}

	// 幂等：第二次运行不改变任何文件
	before := map[string]string{}
	for _, path := range []string{
		filepath.Join(options.Dir, ManagedSkillPath()),
		filepath.Join(options.Dir, ManagedAgentPath()),
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

func TestInstallRefusesUserOwnedSkill(t *testing.T) {
	options := testOptions(t)
	path := filepath.Join(options.Dir, ManagedSkillPath())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# 用户自己的 skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Install(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Errorf("Install() error = %v, want marker refusal", err)
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
	spec, err := ResolveMCP(context.Background(), MCPOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if spec.Host != "http://gitea.example.com:3000" || spec.HostSource != "origin remote" {
		t.Errorf("host = %q (%s)", spec.Host, spec.HostSource)
	}
	if spec.Token != "dev-token" || spec.TokenSource != "GITEA_ACCESS_TOKEN" {
		t.Errorf("token source = %q", spec.TokenSource)
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
	for _, want := range []string{"image: assistant:e2e", "GITEA_STATE_REVIEWER: merger"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow missing %s:\n%s", want, workflow)
		}
	}
	agents := readFile(t, filepath.Join(options.Dir, ManagedAgentPath()))
	if !strings.Contains(agents, "@bot") || !strings.Contains(agents, "`merger`") {
		t.Errorf("AGENTS.md not rendered:\n%s", agents)
	}
	for _, relative := range append([]string{ManagedSkillPath(), ManagedAgentPath()}, ManagedWorkflowPaths()...) {
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
