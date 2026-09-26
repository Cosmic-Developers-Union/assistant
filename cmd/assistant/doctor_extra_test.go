package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/repoinstall"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

// TestPrintLocalFindingsCountsProblemsAndFormatsRows 断言本地体检行的渲染契约：
// 问题数只由 !OK() 决定（doctor 的退出码靠它，多算一次就会让健康仓库以退出码 1
// 结束），行首状态大写且左侧补齐到 9 列，有 Detail 才有破折号后缀（否则操作者
// 会看到悬空的 "— " 却不知道缺什么）。
func TestPrintLocalFindingsCountsProblemsAndFormatsRows(t *testing.T) {
	var out bytes.Buffer
	problems := printLocalFindings(&out, []repoinstall.Finding{
		{Path: ".mcp.json", Status: repoinstall.StatusOK},
		{Path: "AGENTS.md", Status: repoinstall.StatusMissing, Detail: "缺少 assistant 段落"},
		{Path: ".claude/settings.json", Status: repoinstall.StatusUnmanaged, Detail: "非托管键 k"},
	})
	if problems != 2 {
		t.Errorf("problems = %d, want 2（OK 不计问题）", problems)
	}
	got := out.String()
	for _, want := range []string{
		"OK        .mcp.json\n",
		"MISSING   AGENTS.md — 缺少 assistant 段落\n",
		"UNMANAGED .claude/settings.json — 非托管键 k\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q：\n%s", want, got)
		}
	}
	// 状态必须大写：小写状态串若被直接打印，操作者对照文档时无法区分大小写写法。
	if strings.Contains(got, "missing") {
		t.Errorf("状态应为大写：\n%s", got)
	}
	if got := printLocalFindings(&bytes.Buffer{}, nil); got != 0 {
		t.Errorf("空清单 problems = %d, want 0", got)
	}
}

// TestPrintServerFindingsPrefixesPathAndCountsProblems 断言服务端体检行的两处
// 语义差异：路径必须带 "server: " 前缀（本地与服务端检查共用一套渲染格式，
// 不加前缀就分不清问题出在哪一侧），且 skipped 不算问题（权限不足跳过是设计内
// 行为，算成问题会让 doctor 对无管理员权限的操作者恒报失败）。
func TestPrintServerFindingsPrefixesPathAndCountsProblems(t *testing.T) {
	var out bytes.Buffer
	problems := printServerFindings(&out, []status.AuditFinding{
		{Path: "branch protection main", Status: status.AuditStatusOK},
		{Path: "labels", Status: status.AuditStatusSkipped, Detail: "读取被拒"},
		{Path: "branch protection approvals", Status: status.AuditStatusOutdated, Detail: "应为 2 个批准"},
	})
	if problems != 1 {
		t.Errorf("problems = %d, want 1（skipped 与 ok 都不算问题）", problems)
	}
	got := out.String()
	for _, want := range []string{
		"OK        server: branch protection main\n",
		"SKIPPED   server: labels — 读取被拒\n",
		"OUTDATED  server: branch protection approvals — 应为 2 个批准\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q：\n%s", want, got)
		}
	}
	if got := printServerFindings(&bytes.Buffer{}, nil); got != 0 {
		t.Errorf("空清单 problems = %d, want 0", got)
	}
}

// TestSameHostAndFindInstanceByHostMatchLoosely 断言站点比对的宽松语义：
// host 大小写与尾斜杠都不影响判定（同一个人既可能写 https://Gitea.Example.com/
// 也可能写小写无常量），但不同站点绝不能互相命中——findInstanceByHost 的返回值
// 决定 doctor 用哪套 admin 凭据去查服务端，命中错的站点会拿着 A 站令牌查 B 站。
func TestSameHostAndFindInstanceByHostMatchLoosely(t *testing.T) {
	if !sameHost("https://Gitea.Example.com/", "https://gitea.example.com") {
		t.Error("大小写与尾斜杠差异应视为同站")
	}
	if sameHost("https://gitea.example.com", "https://other.example.com") {
		t.Error("不同站点不应视为同站")
	}
	if sameHost("", "https://gitea.example.com") {
		t.Error("空 host 不应命中任何站点")
	}

	file := &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai", Merger: "merge", Repos: []instances.Repo{{Name: "acme/video"}}},
		{Type: instances.ChannelWeixin, BaseURL: "https://qy.example.com"},
	}}
	instance, ok := findInstanceByHost(file, "https://GITEA.example.com/")
	if !ok {
		t.Fatal("尾斜杠与大小写差异应命中 gitea 通道")
	}
	if instance.Host != "https://gitea.example.com" || instance.Reviewer.Name != "ai" || instance.Merger.Name != "merge" {
		t.Errorf("折算的实例视图 = %+v", instance)
	}
	if len(instance.Repos) != 1 || instance.Repos[0].Name != "acme/video" {
		t.Errorf("仓库清单丢失：%+v", instance.Repos)
	}
	// 非 gitea 通道（微信）不能当实例用：它的 Host 为空，命中它会拿到空站点。
	if _, ok := findInstanceByHost(file, ""); ok {
		t.Error("空 host 不应命中")
	}
	if _, ok := findInstanceByHost(&instances.File{}, "https://gitea.example.com"); ok {
		t.Error("无通道时不应命中")
	}
}

// TestResolveServerTargetFromConfigAndProbe 断言服务端体检目标的定位优先级：
// config.json 里已登记的仓库直接命中实例（无需探测网络），remote 与通道同站时
// 按 remote 的仓库名命中，配置里完全没有平台时才退回逐个探测 remote；探测不到
// 时 fullName 保留 --repo 原值以便调用方给出可行动的错误。
func TestResolveServerTargetFromConfigAndProbe(t *testing.T) {
	file := &instances.File{Channels: []instances.Channel{{
		Type:     instances.ChannelGitea,
		Host:     "https://gitea.example.com",
		Reviewer: "ai",
		Merger:   "merge",
		Repos:    []instances.Repo{{Name: "acme/video"}},
	}}}

	// --repo 命中已登记仓库：直接来自配置，且不该走探测。
	probed := 0
	host, fullName, fromConfig := resolveServerTargetWithProbe(file, "acme/video", t.TempDir(),
		func(string) bool { probed++; return true })
	if host != "https://gitea.example.com" || fullName != "acme/video" || !fromConfig {
		t.Errorf("已登记仓库 = %q %q %v", host, fullName, fromConfig)
	}
	if probed != 0 {
		t.Errorf("配置已命中时不应探测 remote（探测 %d 次）", probed)
	}

	// --repo 未登记但只有一个平台：仍按该平台定位（login 只表达作者身份，
	// 当前仓库未必在 repos[] 里），这是 doctor 能检查新仓库的前提。
	host, fullName, fromConfig = resolveServerTargetWithProbe(file, "acme/new", t.TempDir(), func(string) bool { return false })
	if host != "https://gitea.example.com" || fullName != "acme/new" || !fromConfig {
		t.Errorf("单平台 + --repo = %q %q %v", host, fullName, fromConfig)
	}

	// 没有配置文件时：靠探测 remote 判定是否 Gitea。探测为假时退回 --repo 原值，
	// 让调用方打印「未定位到 Gitea 实例」。
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, fromConfig := resolveServerTargetWithProbe(nil, "acme/video", empty, func(string) bool { return false }); fromConfig {
		t.Error("探测失败且无配置时不应声明来自配置")
	}
	host, fullName, fromConfig = resolveServerTargetWithProbe(nil, "acme/video", empty, func(string) bool { return false })
	if fromConfig || host != "" || fullName != "acme/video" {
		t.Errorf("探测失败 = %q %q %v, want 空 host + 原样 fullName", host, fullName, fromConfig)
	}
}

// TestRunDoctorReportsLocalProblemsAndExitsNonZero 断言 doctor 的本地分支端到端：
// 空检出目录必然缺托管文件，命令要以非零错误结束并把缺失清单打出来；这是 CI
// 与本地「先 assistant install」之间的契约——问题被静默吞掉，门禁就形同虚设。
func TestRunDoctorReportsLocalProblemsAndExitsNonZero(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()

	command := newDoctorCommand(new(string))
	// --config / --repo 在 root 命令上注册；这里直接执行子命令，必须自行补上这
	// 两个旗标，否则 auditServer 读旗标时会以「flag accessed but not defined」失败。
	command.Flags().String("config", "", "")
	command.Flags().String("repo", "", "")
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.SetArgs([]string{"--dir", dir})
	// 会话体检依赖真实 claude 可执行文件与凭据；这里只钉本地分支的结论与输出。
	err := command.Execute()
	if err == nil {
		t.Fatal("空目录应报配置问题")
	}
	if !strings.Contains(err.Error(), "发现") || !strings.Contains(err.Error(), "assistant install") {
		t.Errorf("错误信息缺少可行动指引: %v", err)
	}
}

// TestAuditServerSkipsWithoutGiteaTarget 断言服务端体检在定位不到 Gitea 时的
// 语义：不报错，而是返回一条 skipped finding 并给出原因。对 GitHub 托管的仓库
// 跑 doctor 是常见场景，这里报错会让整条命令失败，掩盖真正的本地问题。
func TestAuditServerSkipsWithoutGiteaTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.Flags().String("config", "", "")
	command.Flags().String("repo", "", "")

	findings, target, err := auditServer(command, "", &repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("auditServer: %v", err)
	}
	if target != "" {
		t.Errorf("未定位到目标时 target 应为空，got %q", target)
	}
	if len(findings) != 1 || findings[0].Status != status.AuditStatusSkipped {
		t.Fatalf("findings = %+v, want 一条 skipped", findings)
	}
	if !strings.Contains(findings[0].Detail, "未定位到 Gitea 实例") {
		t.Errorf("Detail 缺少原因: %q", findings[0].Detail)
	}
}

// TestAuditServerReportsMissingCredentialForLocatedRepository 断言「定位到了仓库
// 但拿不到任何凭据」的语义：仍然只是 skipped（doctor 不该因为拿不到凭据就失败），
// 但 target 要点出 host/owner/repo；诊断语要分得清「完全没有凭据（要补 GITEA_HOST
// / GITEA_ACCESS_TOKEN 或登录 admin）」与「有配置但令牌为空」两种情形，否则操作者
// 不知道该往哪一步补。
func TestAuditServerReportsMissingCredentialForLocatedRepository(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// 环境变量模式定位：remote 指向假站点，但既没登记通道凭据也没有
	// GITEA_HOST/GITEA_ACCESS_TOKEN；此时 auditServer 走 config.Load 失败分支。
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	configPath, dir := writeFakeGiteaCheckout(t, fake, "acme/rocket")
	none := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(none, "credentials.json"))

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.Flags().String("config", "", "")
	command.Flags().String("repo", "", "")

	findings, target, err := auditServer(command, configPath, &repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("auditServer: %v", err)
	}
	if target != fake.server.URL+"/acme/rocket" {
		t.Errorf("target = %q, want %q", target, fake.server.URL+"/acme/rocket")
	}
	if len(findings) != 1 || findings[0].Status != status.AuditStatusSkipped {
		t.Fatalf("findings = %+v, want 一条 skipped", findings)
	}
	if !strings.Contains(findings[0].Detail, "缺少凭据") {
		t.Errorf("Detail = %q, want 提到缺少凭据", findings[0].Detail)
	}
}

// TestAuditServerRunsAuditWithAdminToken 断言拿到 admin 用途令牌后真正发起
// 服务端体检，并把实例的 reviewer/merger 身份传给 audit：身份是约定（内容评审
// ai、状态评审/合并 merge），传错等于体检了另一套策略，结论没有意义。
func TestAuditServerRunsAuditWithAdminToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["admin-token"] = "admin"
	configPath, dir := writeFakeGiteaCheckout(t, fake, "acme/rocket")
	// 体检要读分支保护与标签：假站点只为 /api/v1/version 与 /api/v1/user 提供
	// 响应，其余端点 404；audit 会把它记成 finding 而不是错误。
	assistantConfigDir := t.TempDir()
	credentialsPath := filepath.Join(assistantConfigDir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", credentialsPath)
	store := credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: fake.server.URL, User: "admin", Purpose: credentials.PurposeAdmin,
		Token: "admin-token", TokenName: "assistant",
	})
	if err := credentials.Save(credentialsPath, &store); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.Flags().String("config", "", "")
	command.Flags().String("repo", "", "")

	findings, target, err := auditServer(command, configPath, &repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("auditServer: %v", err)
	}
	if target != fake.server.URL+"/acme/rocket" {
		t.Errorf("target = %q", target)
	}
	if len(findings) == 0 {
		t.Fatal("体检应产出 findings（假站点缺分支保护/标签，应报出问题或跳过）")
	}
}

// TestRunDoctorPrintsServerTargetHeaderWhenLocated 断言定位成功时打出的目标行
// 格式：操作者据此把 findings 对应到具体站点与仓库；行前带 "#" 让它读起来像
// 标题而不是一条 finding。
func TestRunDoctorPrintsServerTargetHeaderWhenLocated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["admin-token"] = "admin"
	configPath, dir := writeFakeGiteaCheckout(t, fake, "acme/rocket")
	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", credentialsPath)
	store := credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: fake.server.URL, User: "admin", Purpose: credentials.PurposeAdmin,
		Token: "admin-token", TokenName: "assistant",
	})
	if err := credentials.Save(credentialsPath, &store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.Flags().String("config", "", "")
	command.Flags().String("repo", "", "")

	// 本地体检在空目录下必然发现问题，所以这里只断言目标行被打印出来。
	_ = runDoctor(command, configPath, &repoToolOptions{Dir: dir}, 2, false)
	want := "# 服务端检查目标：" + fake.server.URL + "/acme/rocket\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("输出缺少目标行 %q：\n%s", want, out.String())
	}
}

// TestPrintSessionFindingsReportsCredentialSource 断言会话体检的凭据分支：
// 没有 config.json 时是 SKIPPED（不阻断），provider 配了 api_key 时是 OK 并
// 点出凭据来源；凭据不再来自 ~/.claude 登录态，所以「来源」必须显式打出来，
// 否则操作者无法判断会话会拿哪个 key 去调模型。
func TestPrintSessionFindingsReportsCredentialSource(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var out bytes.Buffer
	// 不存在的配置路径：退化成「没有 config.json，跳过」。
	printSessionFindings(&out, filepath.Join(t.TempDir(), "absent.json"))
	if !strings.Contains(out.String(), "会话运行时 claude") {
		t.Errorf("输出缺少 claude 运行时检查：\n%s", out.String())
	}
	if !strings.Contains(out.String(), "SKIPPED") {
		t.Errorf("无 config.json 时应 SKIPPED：\n%s", out.String())
	}

	// 有 config.json 且 provider 明确带 api_key：应报 OK 并给出来源。
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{
		DefaultProvider: "work",
		Providers: map[string]instances.Provider{
			"work": {APIKey: "sk-test-1234", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.example.com"}},
		},
		Runtimes: map[string]instances.Runtime{"main": {Root: t.TempDir()}},
	}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	problems := printSessionFindings(&out, configPath)
	if !strings.Contains(out.String(), "会话 AI 凭据") {
		t.Fatalf("输出缺少凭据检查：\n%s", out.String())
	}
	if strings.Contains(out.String(), "没有 api_key") {
		t.Errorf("已配 api_key 的 provider 不应报缺失：\n%s", out.String())
	}
	_ = problems
}

// TestRepoOptionsMapsEveryFieldAndLogsToCommand 断言 repoinstall 选项的映射：
// 每个字段都要落到 repoinstall.Options（漏映射会让 --dry-run / --image 之类的
// 旗标静默失效），日志行必须写进命令的输出流而不是进程 stdout——否则
// --dry-run 的输出无法被调用方捕获或重定向。
func TestRepoOptionsMapsEveryFieldAndLogsToCommand(t *testing.T) {
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	command.SetErr(&bytes.Buffer{})

	options := &repoToolOptions{
		Dir:          "/tmp/repo",
		Tools:        []string{"claude", "codex"},
		CodexPath:    "/tmp/codex.toml",
		Image:        "ghcr.io/example/review:1",
		SkillsSource: "github:example/skills",
		DryRun:       true,
	}
	mapped := options.repoOptions(command)
	if mapped.Dir != options.Dir || mapped.CodexConfigPath != options.CodexPath ||
		mapped.Image != options.Image || mapped.SkillsSource != options.SkillsSource ||
		mapped.DryRun != options.DryRun || len(mapped.Tools) != 2 || mapped.Tools[1] != "codex" {
		t.Errorf("映射结果 = %+v", mapped)
	}
	if mapped.Log == nil {
		t.Fatal("Log 必须接上（否则 install 提示被吞）")
	}
	mapped.Log("写入 %s", "AGENTS.md")
	if !strings.Contains(out.String(), "写入 AGENTS.md\n") {
		t.Errorf("日志未写进命令输出：%q", out.String())
	}
}

// TestFakeGiteaVersionEndpointIsProbed 断言假 Gitea 的 /api/v1/version 探针
// 语义与真站点一致：status.ProbeGitea 靠它判定 remote 是否 Gitea，版本端点坏掉
// 时探测必须为假（doctor 于是在多 remote 场景下逐个探测并跳过非 Gitea 站点）。
func TestFakeGiteaVersionEndpointIsProbed(t *testing.T) {
	fake := newFakeGitea(t)
	if !status.ProbeGitea(t.Context(), fake.server.URL) {
		t.Error("健康假站点应被探测为 Gitea")
	}
	response, err := http.Get(fake.server.URL + "/api/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("version 端点 HTTP %d, want 200", response.StatusCode)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	if status.ProbeGitea(t.Context(), dead.URL) {
		t.Error("版本端点 500 时不应探测为 Gitea（否则会把普通站点当实例）")
	}
}

// writeFakeGiteaCheckout 造一个「确实在跑的假站点 + 已登记它的 config.json +
// 指向它的 git 检出」，返回 (configPath, repoDir)。doctor 的服务端分支靠 origin
// remote 的 URL 定位站点（resolveServerTarget 读 git remote 再按 host 命中通道），
// 只写 config.json 而目录里没有 remote 会退化到「未定位到 Gitea 实例」，测不出
// admin 凭据与体检这条链路。
func writeFakeGiteaCheckout(t *testing.T, fake *fakeGitea, repoName string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	// remote URL 必须含路径，dispatcher 要从中解析出 owner/repo。
	runGit(t, dir, "remote", "add", "origin", fake.server.URL+"/"+repoName+".git")
	return writeFakeGiteaConfig(t, fake, repoName), dir
}

// runGit 在 dir 里执行一条 git 命令；失败即终止测试（夹具损坏时继续跑只会得到
// 误导性的「未定位到实例」）。
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// TestLoadOptionalConfigSurfacesBrokenDotEnv 断言配置文件旁边的 .env 坏掉时
// loadOptionalConfig 直接上抛：config init 的全部写入都以「现有配置能读进来」为
// 前提，跳过这层检查会让补全后的配置与 .env 里的引用对不上，写出一个启动即报错的
// 配置。这里用「.env 是个目录」触发 LoadBeside 的非普通文件错误。
func TestLoadOptionalConfigSurfacesBrokenDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, &instances.File{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptionalConfig(path); err == nil {
		t.Fatal(".env 不是普通文件时应上抛错误")
	}
}

// TestLoadOptionalConfigParsesEmptySkeleton 断言 config new 产出的空骨架能被
// loadOptionalConfig 读回来：这种文件过不了语义校验（没有 instances/runtimes），
// 但对「补全」有效，所以语义校验失败要退回宽松解析而不是报错——否则
// `assistant config new` 之后紧跟的 `config init` 会拒绝补全自己刚写的文件。
func TestLoadOptionalConfigParsesEmptySkeleton(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := loadOptionalConfig(path)
	if err != nil {
		t.Fatalf("空骨架应可读：%v", err)
	}
	if file == nil {
		t.Fatal("file = nil, want 宽松解析出的配置")
	}
}

// TestPrintSessionFindingsSkipsInvalidConfig 断言配置语义无效时凭据一项报 SKIPPED
// 而不是崩掉或误报凭据就绪：resolveInstanceFile 会先跑一遍 File.Validate（它已经
// 覆盖了密钥引用与 provider.Resolve 的全部错误路径），所以无效配置根本到不了
// 「解析 provider 覆盖」那一步。
//
// 这条断言同时给 doctor.go 的 UNMANAGED 分支（printSessionFindings 里
// EffectiveOverrides 报错的出口）留证：能通过 resolveInstanceFile 的文件必然能解析，
// 该分支不可达，属死代码，故不追覆盖率。
func TestPrintSessionFindingsSkipsInvalidConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{
		DefaultProvider: "openai",
		Providers: map[string]instances.Provider{
			// 预设要求必须给 ANTHROPIC_BASE_URL，这里故意不给。
			"openai": {APIKey: "sk-test"},
		},
		Runtimes: map[string]instances.Runtime{"main": {Root: t.TempDir()}},
	}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	problems := printSessionFindings(out, configPath)
	if !strings.Contains(out.String(), "SKIPPED") {
		t.Errorf("无效配置应报 SKIPPED，输出：\n%s", out.String())
	}
	if problems != 0 {
		t.Errorf("problems = %d, want 0（凭据一项跳过不计问题）", problems)
	}
}
