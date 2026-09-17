package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/claudecfg"
	"assistant/internal/dispatcher"
	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

// run.yaml 存在时 run 完全按它运行：站点/仓库来自 monitor，令牌回退凭据库，
// 落点与评审工作区按 root/repos-dir/review-*；config.json 退为账号身份层
// （这里的 config.json 没有 instances，不应阻断运行）。
func TestResolveDispatchTargetsFromRunYaml(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	// run.yaml 模式下 config.json 只承担账号身份；instances 可为空（weixin 兜底
	// 通过校验），不应阻断运行
	if err := instances.Save(configPath, &instances.File{Weixin: &instances.Weixin{}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", configPath)
	withReviewCredentialFor(t, configPath, "https://gitea.example.com")

	runYaml := `
monitor:
  https://gitea.example.com:
    repos:
      - acme/rocket
      - acme/lab
root: ${XDG_DATA_HOME:-$HOME/.local/share}/Cosmic-Developers-Union/assistant
repos-dir: $root/repos
review-root: /tmp
review-name-template: "${instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"
`
	runPath := filepath.Join(configDir, "run.yaml")
	if err := os.WriteFile(runPath, []byte(runYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	targets, err := resolveDispatchTargets(command, "", configPath, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveDispatchTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	first := targets[0]
	if first.config.Host != "https://gitea.example.com" || first.config.Repository.FullName() != "acme/rocket" {
		t.Errorf("首个目标 = %s %s", first.config.Host, first.config.Repository.FullName())
	}
	if first.instance.Host != "https://gitea.example.com" {
		t.Errorf("instance.Host = %q（受管克隆与 daemon 状态按它键控）", first.instance.Host)
	}
	if !first.managed {
		t.Error("run.yaml 目标应是受管克隆")
	}
	// 落点：root 决定的 repos 根
	wantRepo := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "repos", "gitea.example.com", "acme", "rocket")
	if first.repoDir != wantRepo {
		t.Errorf("repoDir = %q, want %q", first.repoDir, wantRepo)
	}
	// 日志/锁在检出之外的状态目录
	wantState := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "state", "gitea.example.com", "acme", "rocket")
	if first.config.LogDir != filepath.Join(wantState, "logs") || first.config.LockFile != filepath.Join(wantState, "dispatcher.lock") {
		t.Errorf("LogDir = %q LockFile = %q", first.config.LogDir, first.config.LockFile)
	}
	if !first.config.SyncMirror {
		t.Error("受管克隆应恒开镜像同步")
	}
	// 评审工作区：review-root + 命名模板展开（模板已含站点与仓库）
	wantWorktree := filepath.Join("/tmp", "gitea.example.com-acme--rocket", "worktrees")
	if first.config.WorktreeRoot != wantWorktree {
		t.Errorf("WorktreeRoot = %q, want %q", first.config.WorktreeRoot, wantWorktree)
	}
	// monitor 未写 token 时回退凭据库
	if first.config.AccessToken == "" {
		t.Error("AccessToken 不应为空（回退凭据库）")
	}
	if !strings.Contains(stderr.String(), "monitor[https://gitea.example.com]") {
		t.Errorf("stderr 缺少 monitor 日志：%s", stderr.String())
	}

	// --repo 过滤仍可用
	filtered, err := resolveDispatchTargets(command, "acme/lab", configPath, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("--repo 过滤: %v", err)
	}
	if len(filtered) != 1 || filtered[0].config.Repository.FullName() != "acme/lab" {
		t.Fatalf("--repo 过滤结果 = %+v", filtered)
	}

	// 不在 run.yaml 里的仓库报错
	if _, err := resolveDispatchTargets(command, "acme/other", configPath, &dispatcherOptions{}); err == nil {
		t.Error("不在 monitor repos 中的仓库应报错")
	}
}

// 未 setup 的实例（无 repos）应跳过并提示，不阻断其他实例。
func TestResolveDispatchTargetsSkipsInstancesWithoutRepos(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://gitea.aicler.com"},
		{
			Host:  "https://gitea.mms.vincentge.top",
			Repos: []instances.Repo{{Name: "Ge/assistant", Dir: dir}},
		},
	}}
	file.Normalize()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, file); err != nil {
		t.Fatalf("save config: %v", err)
	}

	withReviewCredentialFor(t, path, "https://gitea.mms.vincentge.top")

	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	targets, err := resolveDispatchTargets(command, "", path, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveDispatchTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].config.Repository.FullName() != "Ge/assistant" {
		t.Fatalf("targets = %+v, want 仅 Ge/assistant", targets)
	}
	if !strings.Contains(stderr.String(), "跳过 https://gitea.aicler.com") {
		t.Errorf("stderr = %q, 缺少跳过提示", stderr.String())
	}
}

// 未配置 repo.dir 的仓库使用受管克隆落点：检出、日志/锁都在数据目录（不随
// 当前目录漂移），worktree 固定在 /tmp。
func TestResolveInstanceTargetUsesManagedDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(command, &instances.File{}, "", instance, instances.Repo{Name: "acme/repo"}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	data := os.Getenv("XDG_DATA_HOME")
	repoDir := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "repos", "gitea.example.com", "acme", "repo")
	stateDir := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "state", "gitea.example.com", "acme", "repo")
	if !target.managed || target.repoDir != repoDir {
		t.Errorf("managed=%v repoDir=%q, want %q", target.managed, target.repoDir, repoDir)
	}
	if target.config.LogDir != filepath.Join(stateDir, "logs") {
		t.Errorf("LogDir = %q", target.config.LogDir)
	}
	if target.config.LockFile != filepath.Join(stateDir, "dispatcher.lock") {
		t.Errorf("LockFile = %q", target.config.LockFile)
	}
	wantWorktree := filepath.Join(os.TempDir(), "agent-dispatcher", "gitea.example.com-acme-repo", "worktrees")
	if target.config.WorktreeRoot != wantWorktree {
		t.Errorf("WorktreeRoot = %q, want %q", target.config.WorktreeRoot, wantWorktree)
	}
	if !target.config.SyncMirror {
		t.Error("受管克隆应恒开镜像同步")
	}
}

// 显式配置 dir 的共享检出保持原语义：日志/锁在检出内，不受管、不自动镜像。
func TestResolveInstanceTargetExplicitDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	dir := t.TempDir()
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(command, &instances.File{}, "", instance, instances.Repo{Name: "acme/repo", Dir: dir}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.managed || target.repoDir != dir {
		t.Errorf("managed=%v repoDir=%q, want 非受管 %q", target.managed, target.repoDir, dir)
	}
	if target.config.LogDir != filepath.Join(dir, "logs") || target.config.LockFile != filepath.Join(dir, "dispatcher.lock") {
		t.Errorf("LogDir/LockFile = %q/%q", target.config.LogDir, target.config.LockFile)
	}
	if target.config.SyncMirror {
		t.Error("显式 dir 的共享检出不应默认镜像同步")
	}
}

// --repo-dir 是单目标显式覆盖：按共享检出处理，多目标时必须配合 --repo。
func TestRepoDirOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	dir := t.TempDir()
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(
		command, &instances.File{}, "", instance, instances.Repo{Name: "acme/repo"}, &dispatcherOptions{RepoDir: dir})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.managed || target.repoDir != dir {
		t.Errorf("--repo-dir 覆盖应是非受管检出：managed=%v dir=%q", target.managed, target.repoDir)
	}

	// 配置里两个仓库 + --repo-dir 且未用 --repo 收敛 → 报错
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{Instances: []instances.Instance{{
		Host:     instance.Host,
		Reviewer: instance.Reviewer,
		Merger:   instance.Merger,
		Repos:    []instances.Repo{{Name: "acme/one"}, {Name: "acme/two"}},
	}}}
	file.Normalize()
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	_, err = resolveDispatchTargets(command, "", configPath, &dispatcherOptions{RepoDir: dir})
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("err = %v, want 提示用 --repo 限定", err)
	}
}

// 仓库缺少 install 产物**不再是门槛**：MCP 与评审协议由 assistant 注入，两个仓库
// 都就绪，只提示一句缺什么（受管克隆失败才跳过）。
func TestPrepareManagedTargetsIgnoresScaffolding(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)

	targetFor := func(name string) dispatchTarget {
		instance := instances.Instance{
			Host:     "https://gitea.example.com",
			Reviewer: instances.Account{Name: "ai"},
			Merger:   instances.Account{Name: "merge"},
		}
		target, err := resolveInstanceTarget(command, &instances.File{}, "", instance, instances.Repo{Name: name}, &dispatcherOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return target
	}

	provisioned := targetFor("acme/provisioned")
	if err := os.MkdirAll(filepath.Join(provisioned.repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(provisioned.repoDir, ".claude", "skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".mcp.json", ".claude/settings.json", ".claude/skills/review/SKILL.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(provisioned.repoDir, relative), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bare := targetFor("acme/bare")
	if err := os.MkdirAll(filepath.Join(bare.repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	targets := prepareManagedTargets(command, []dispatchTarget{provisioned, bare})
	for index, target := range targets {
		if target.skipReason != "" {
			t.Errorf("仓库 %d 不应因缺脚手架被跳过：%s", index, target.skipReason)
		}
	}
	if !strings.Contains(stderr.String(), "acme/bare 没有 .mcp.json") ||
		!strings.Contains(stderr.String(), "评审照常进行") {
		t.Errorf("日志缺少缺脚手架的提示：%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "acme/provisioned 没有") {
		t.Errorf("已装脚手架的仓库不该有提示：%s", stderr.String())
	}
	if readyList := readyTargets(targets); len(readyList) != 2 {
		t.Errorf("readyTargets = %+v", readyList)
	}
	if summary := skipSummary(targets); summary != "" {
		t.Errorf("skipSummary = %q", summary)
	}
}

// provider 逐级回退写进运行配置（仅运行时覆盖）：repo > instance > 全局默认。
func TestResolveInstanceTargetProviderCascade(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	command := &cobra.Command{}
	file := &instances.File{
		DefaultProvider: "global",
		Optimizations:   instances.Provider{Env: map[string]string{"OPTIMIZED": "1", "SCOPE": "optimized"}},
		Providers: map[string]instances.Provider{
			"global":   {Env: map[string]string{"SCOPE": "global"}},
			"instance": {Env: map[string]string{"SCOPE": "instance"}},
			"repo":     {Env: map[string]string{"SCOPE": "repo"}},
		},
	}
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Provider: "instance",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}

	repo := instances.Repo{Name: "acme/repo", Provider: "repo"}
	target, err := resolveInstanceTarget(command, file, "", instance, repo, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.config.ProviderName != "repo" || target.config.Provider.Env["SCOPE"] != "repo" {
		t.Errorf("repo 级 provider 未生效：%q %+v", target.config.ProviderName, target.config.Provider.Env)
	}
	if target.config.Provider.Env["OPTIMIZED"] != "1" {
		t.Errorf("全局优化点应打底：%+v", target.config.Provider.Env)
	}
	if target.config.Optimizations.Env["SCOPE"] != "optimized" {
		t.Errorf("全局优化点层未记录：%+v", target.config.Optimizations.Env)
	}

	repo.Provider = ""
	target, err = resolveInstanceTarget(command, file, "", instance, repo, &dispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if target.config.ProviderName != "instance" || target.config.Provider.Env["SCOPE"] != "instance" {
		t.Errorf("instance 级 provider 未生效：%q %+v", target.config.ProviderName, target.config.Provider.Env)
	}

	instance.Provider = ""
	target, err = resolveInstanceTarget(command, file, "", instance, repo, &dispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if target.config.ProviderName != "global" || target.config.Provider.Env["SCOPE"] != "global" {
		t.Errorf("全局 provider 未生效：%q %+v", target.config.ProviderName, target.config.Provider.Env)
	}

	// 引用了未定义的 provider 在配置校验期就会被拒绝
	file.DefaultProvider = "missing"
	file.Instances = []instances.Instance{instance}
	file.Weixin = &instances.Weixin{BotToken: "t"}
	file.Normalize()
	if err := file.Validate(); err == nil {
		t.Error("未定义的 provider 引用应报错")
	}
}

// 会话前提自检与仓库内容无关：claude 缺失、没有 AI 凭据都要在启动日志里说清楚
// （全部仓库被跳过时这是唯一的诊断线索）。
func TestCheckSessionRuntimeReportsPrerequisites(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo '2.1.270 (Claude Code)';;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(dir, "claude-config")

	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)
	var logged []string
	log := func(line string) { logged = append(logged, line) }

	checkSessionRuntime(command, []dispatchTarget{{
		config: dispatcher.Config{ClaudeBin: bin, SessionDir: sessionDir, ProviderName: "opencode"},
	}}, log)
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "2.1.270") || !strings.Contains(joined, sessionDir) {
		t.Errorf("缺少 claude/配置根日志：%s", joined)
	}
	if !strings.Contains(stderr.String(), "没有可用的 AI 凭据") || !strings.Contains(stderr.String(), "api_key") {
		t.Errorf("缺少凭据告警：%s", stderr.String())
	}

	// provider 提供凭据后会真的探一次端点：这里用假端点，避免测试联网
	var lastRequest string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lastRequest = request.URL.Path
		_, _ = writer.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	logged = nil
	stderr.Reset()
	checkSessionRuntime(command, []dispatchTarget{{
		config: dispatcher.Config{
			ClaudeBin:    bin,
			SessionDir:   sessionDir,
			ProviderName: "opencode",
			Provider: claudecfg.Overrides{
				Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_BASE_URL": server.URL},
			},
		},
	}}, log)
	if stderr.Len() != 0 {
		t.Errorf("有凭据且端点连通时不该告警：%s", stderr.String())
	}
	joined = strings.Join(logged, "\n")
	if !strings.Contains(joined, "provider env ANTHROPIC_AUTH_TOKEN") || !strings.Contains(joined, "端点连通") {
		t.Errorf("缺少凭据来源/端点日志：%v", logged)
	}
	if lastRequest != "/v1/messages" {
		t.Errorf("自检请求路径 = %q", lastRequest)
	}

	// 端点拒绝凭据（401）：启动就告警，并给出端点/密钥配套的提示
	rejecting := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	t.Cleanup(rejecting.Close)
	logged = nil
	stderr.Reset()
	checkSessionRuntime(command, []dispatchTarget{{
		config: dispatcher.Config{
			ClaudeBin:    bin,
			SessionDir:   sessionDir,
			ProviderName: "opencode",
			Provider: claudecfg.Overrides{
				Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "bad", "ANTHROPIC_BASE_URL": rejecting.URL},
			},
		},
	}}, log)
	if !strings.Contains(stderr.String(), "凭据自检失败") || !strings.Contains(stderr.String(), "HTTP 401") {
		t.Errorf("缺少自检失败告警：%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "api.minimaxi.com") {
		t.Errorf("缺少端点/密钥配套提示：%s", stderr.String())
	}

	// claude 不存在：给出可诊断的警告而不是等会话失败
	logged = nil
	stderr.Reset()
	checkSessionRuntime(command, []dispatchTarget{{
		config: dispatcher.Config{ClaudeBin: filepath.Join(dir, "absent"), SessionDir: sessionDir},
	}}, log)
	if !strings.Contains(stderr.String(), "找不到 claude 可执行文件") {
		t.Errorf("缺少 claude 缺失告警：%s", stderr.String())
	}
}
