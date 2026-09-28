package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"
	"github.com/spf13/cobra"
)

// seedGiteaConfig 把一份只有指定 gitea 通道的配置写到临时目录，并让
// ASSISTANT_CONFIG 指向它（缺省入口，不必记着传 --config）。
func seedGiteaConfig(t *testing.T, channels ...instances.Channel) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{Channels: channels, Runtimes: map[string]instances.Runtime{"main": {MainAgent: "main"}}}
	if err := instances.Save(path, file); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", path)
	return path
}

// giteaChannelForTest 造一个合法的 gitea 通道（reviewer/merger 齐全，可过 Validate）。
func giteaChannelForTest(host string, repos ...string) instances.Channel {
	entries := make([]instances.Repo, 0, len(repos))
	for _, name := range repos {
		entries = append(entries, instances.Repo{Name: name})
	}
	return instances.Channel{
		Type: instances.ChannelGitea, Host: host,
		Reviewer: instances.DefaultReviewerName, Merger: instances.DefaultMergerName,
		Repos: entries,
	}
}

// TestSessionsMCPReportsMissingServerConfig 断言同机没有 serve.json 时 sessions MCP
// 在 stderr 给出「未发现记录库服务端配置」的提示，并同时打印实际会被读取的
// sessions-remote.json 路径。
//
// 这是「MCP 装上了但没接记录库」这一最常见形态：agent 的工具调用只会回一个
// 未配置错误，操作者若看不到这句话就无从知道该去配哪个文件。路径必须打印出来，
// 因为使用者手上没有别的地方能查到它。
func TestSessionsMCPReportsMissingServerConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_SESSIONS_URL", "")
	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, minimalConfig()); err != nil {
		t.Fatal(err)
	}

	command := newSessionsMCPCommand(&configPath)
	var errOut bytes.Buffer
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&errOut)
	// 真实运行会在这里起 stdio MCP 循环；用 stdin 立即 EOF 让它干净返回。
	command.SetIn(strings.NewReader(""))
	command.SetArgs([]string{})
	// MCP 循环读 stdin：EOF 后应当自己结束，不会阻塞用例
	if err := command.Execute(); err != nil {
		t.Fatalf("无服务端配置时不该报错退出：%v", err)
	}
	text := errOut.String()
	if !strings.Contains(text, "提示：未发现记录库服务端配置") {
		t.Errorf("应提示未发现服务端配置：\n%s", text)
	}
	if !strings.Contains(text, filepath.Join(dir, sessionstore.RemoteFile)) {
		t.Errorf("应点明 remote 配置文件路径：\n%s", text)
	}
}

// TestSessionsMCPStaysQuietWhenRemoteConfigExists 断言配置目录里已有 serve.json
// 时不打印那句提示。
//
// 提示一旦在服务端配好的机器上照样出现，就会变成每次会话都刷屏的噪音，操作者
// 很快会学会无视它——那样真正缺配置时反而没人看。
func TestSessionsMCPStaysQuietWhenRemoteConfigExists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_SESSIONS_URL", "")
	if err := os.WriteFile(filepath.Join(dir, sessionstore.ServeFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, minimalConfig()); err != nil {
		t.Fatal(err)
	}

	command := newSessionsMCPCommand(&configPath)
	var errOut bytes.Buffer
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&errOut)
	command.SetIn(strings.NewReader(""))
	command.SetArgs([]string{})
	if err := command.Execute(); err != nil {
		t.Fatalf("有服务端配置时不该报错：%v", err)
	}
	if strings.Contains(errOut.String(), "未发现记录库服务端配置") {
		t.Errorf("服务端配置存在时不该提示未配置：\n%s", errOut.String())
	}
}

// TestSessionsMCPReportsUnresolvableConfigDir 断言配置落点取不出来（当前目录已被
// 删除）时命令直接失败，而不是拿一个空目录去继续跑 MCP。
//
// 此时程序根本不知道记录库在哪里；继续跑下去 agent 会拿到一连串「未配置」错误，
// 而真正的原因（工作目录没了）被完全掩盖。
func TestSessionsMCPReportsUnresolvableConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	command := newSessionsMCPCommand(new(string))
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetIn(strings.NewReader(""))
	command.SetArgs([]string{})
	if err := command.Execute(); err == nil {
		t.Error("配置目录取不出来时应报错")
	}
}

// TestSelectRepoGiteaRejectsRepoOnMultipleHosts 断言同一仓库名登记在两个 gitea
// 站点上时选择报错并点名仓库，而不是随便挑一个站点下笔。
//
// 这是多站点用户最容易踩的一脚：两个站点上都有同名仓库（fork/镜像），挑错了
// 站点会把仓库登记写到错误的平台上，之后所有的评审调度都对着错误的 host 跑。
func TestSelectRepoGiteaRejectsRepoOnMultipleHosts(t *testing.T) {
	file := &instances.File{Channels: []instances.Channel{
		giteaChannelForTest("https://a.example.com", "acme/video"),
		giteaChannelForTest("https://b.example.com", "acme/video"),
	}}

	// 无 --host、无 remote 提示：只能凭「哪些站点登记了这个仓库」判断，而它是歧义的
	_, err := selectRepoGitea(file, "", "acme/video")
	if err == nil {
		t.Fatal("仓库登记在多个平台时应报错")
	}
	for _, want := range []string{"acme/video", "登记在多个平台", "--host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q：%v", want, err)
		}
	}

	// 只有一个站点登记时应当选中它，证明上面的报错确实来自「多个」而不是别的原因
	single := &instances.File{Channels: []instances.Channel{
		giteaChannelForTest("https://a.example.com", "acme/video"),
		giteaChannelForTest("https://b.example.com", "acme/other"),
	}}
	channel, err := selectRepoGitea(single, "", "acme/video")
	if err != nil {
		t.Fatalf("唯一登记站点应被选中：%v", err)
	}
	if channel.Host != "https://a.example.com" {
		t.Errorf("Host = %q, want https://a.example.com", channel.Host)
	}

	// --host 显式指定时必须压过歧义判断，否则多站点用户没有任何出路
	channel, err = selectRepoGitea(file, "https://b.example.com", "acme/video")
	if err != nil {
		t.Fatalf("--host 应能指定歧义站点：%v", err)
	}
	if channel.Host != "https://b.example.com" {
		t.Errorf("--host 未生效，Host = %q", channel.Host)
	}
}

// TestSaveConfigReportsInvalidFile 断言配置过不了校验时 saveConfig 原样返回校验
// 错误且不落盘，而不是把非法配置写出去。
//
// saveConfig 是所有登记命令（login/repos/setup/init/migrate）写配置的唯一出口；
// 它若在校验失败时仍然 Save，用户会得到一份连自己都启动不了的 config.json，而
// 命令却报告「已登记」。
func TestSaveConfigReportsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// host 是相对路径：Normalize 只会去尾斜杠，ValidateHost 必然拒绝
	file := &instances.File{Channels: []instances.Channel{
		giteaChannelForTest("not-a-site", "acme/video"),
	}}

	err := saveConfig(file, path)
	if err == nil {
		t.Fatal("非法配置应让 saveConfig 报错")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("错误应点明 host 不合法：%v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("校验失败时不该写出配置文件")
	}

	// 合法配置必须真的落盘，证明上面的「没写出」来自校验而不是别的环节
	valid := &instances.File{Channels: []instances.Channel{
		giteaChannelForTest("https://ok.example.com", "acme/video"),
	}}
	if err := saveConfig(valid, path); err != nil {
		t.Fatalf("合法配置应写入成功：%v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "https://ok.example.com") {
		t.Errorf("配置未写出站点：\n%s", data)
	}
}

// TestLoginListChannelReportsUnparsableConfig 断言 login list --type 在配置无法
// 解析时报错退出，而不是当作「没有该类型通道」打印「未配置」。
//
// 二者对操作者的指向完全相反：前者要去修 config.json 的语法，后者会让人以为只是
// 漏加了 telegram 条目，于是在一份根本读不出来的配置上反复加通道。
func TestLoginListChannelReportsUnparsableConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runLoginList(t, configPath, "--type", "telegram")
	if err == nil {
		t.Fatalf("坏配置应报错：\n%s", out)
	}
	if strings.Contains(out, "未配置 telegram 通道") {
		t.Errorf("不该把坏配置报成未配置通道：\n%s", out)
	}
}

// TestInstallCommandsReportUnwritableTarget 断言三条仓库脚手架命令在目标目录写不
// 进去时报错退出，而不是把「什么都没写成」报成成功。
//
// 空目录本身不是失败（install 就是用来初始化空目录的），真正的失败面是写盘被挡：
// 这里让 skills CLI 必然失败（PATH 里没有 bunx），install 必须在技能安装处中止，
// 而不是继续往下走、最后报告「已安装」。dry-run 走的是更早的分流分支，比不到这
// 条真实失败面。
func TestInstallCommandsReportUnwritableTarget(t *testing.T) {
	dir := t.TempDir()
	// 把 codex 配置指到临时目录：不能让它去改真实的 ~/.codex/config.toml
	codexPath := filepath.Join(dir, "codex-config.toml")
	// PATH 清空 → 找不到 bunx → 技能安装必然失败（否则本用例会真的装技能）
	t.Setenv("PATH", t.TempDir())

	command := newInstallCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--dir", dir, "--codex-config", codexPath})
	if err := command.Execute(); err == nil {
		t.Errorf("技能装不上时 install 应报错：\n%s", out.String())
	}

	// gitea-actions 只写 workflow：目标路径被目录占住时写入必然失败
	blocked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(blocked, ".gitea", "workflows", "assistant.yml"), 0o755); err != nil {
		t.Fatal(err)
	}
	actions := newInstallGiteaActionsCommand()
	out.Reset()
	actions.SetOut(&out)
	actions.SetErr(&out)
	actions.SetArgs([]string{"--dir", blocked})
	if err := actions.Execute(); err == nil {
		t.Errorf("workflow 写不进去时 install gitea-actions 应报错：\n%s", out.String())
	}

	// uninstall：清理 skills CLI 之后还要改 codex 配置，同样必须受限在临时目录
	uninstall := newUninstallCommand()
	out.Reset()
	uninstall.SetOut(&out)
	uninstall.SetErr(&out)
	uninstall.SetArgs([]string{"--dir", dir, "--codex-config", codexPath})
	if err := uninstall.Execute(); err != nil {
		t.Errorf("空目录上 uninstall 应干净完成（只清理不存在的托管内容）：%v\n%s", err, out.String())
	}
}

// TestLoginRemoveReportsMissingCredentialsDir 断言凭据库路径定位不到时 login remove
// 报错，而不是把「没有本地凭据」当成结论去删配置条目。
//
// remove 会先删配置里的平台条目再清凭据；若凭据路径取不出来就静默跳过，命令会
// 报「已移除」而凭据仍然留在某个未知位置——用户以为登出干净了。
func TestLoginRemoveReportsMissingCredentialsDir(t *testing.T) {
	configPath := seedGiteaConfig(t, giteaChannelForTest("https://rm.example.com", "acme/video"))
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/path")

	command := newLoginRemoveCommand(&configPath, nil)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"https://rm.example.com"})
	if err := command.Execute(); err == nil {
		t.Errorf("凭据库路径取不出来时应报错：\n%s", out.String())
	}
}

// TestLoginRemoveReportsCorruptCredentialStore 断言凭据库损坏时 login remove 报错，
// 而不是当作「该站点没有凭据」只清掉配置条目。
//
// 这是最糟的一半完成：配置里的平台没了、凭据文件里的令牌还在，用户以为撤销了
// 访问，而站点上那条令牌仍然有效。
func TestLoginRemoveReportsCorruptCredentialStore(t *testing.T) {
	configPath := seedGiteaConfig(t, giteaChannelForTest("https://rm.example.com", "acme/video"))
	storePath := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)
	if err := os.WriteFile(storePath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := newLoginRemoveCommand(&configPath, nil)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"https://rm.example.com"})
	err := command.Execute()
	if err == nil {
		t.Fatalf("凭据库损坏时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("错误应点明坏掉的凭据库：%v", err)
	}
	// 报错发生在清凭据之前，配置里的平台条目也不该被动过
	file, loadErr := instances.Load(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(giteaChannels(file)) != 1 {
		t.Errorf("失败时不该改写配置：%+v", file.Channels)
	}
}

// TestLoginTokenRefreshReportsUnparsableConfig 断言 token refresh 在配置无法解析时
// 报错，而不是先去做一次注定无法保存的站点轮换。
//
// 轮换是有副作用的写操作：它会在站点上删掉旧令牌再建新的。若配置读不出来却继续，
// 站点上的令牌被换掉了，本地却因为写不进配置而仍然持有已失效的旧令牌。
func TestLoginTokenRefreshReportsUnparsableConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))

	command := newLoginTokenRefreshCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"https://broken.example.com", "mcp", "--user", "ge", "--password", "pw"})
	if err := command.Execute(); err == nil {
		t.Errorf("坏配置应让 refresh 报错：\n%s", out.String())
	}
}

// TestResolveConfigDirReportsUnresolvablePath 断言配置落点取不出来时 resolveConfigDir
// 报错，而不是返回一个空目录字符串。
//
// 该函数是 serve / session / sessions MCP 三条命令共用的入口；空目录会让这些命令
// 把产物写到进程某个未知位置，用户重启 daemon 后找不到任何记录。
func TestResolveConfigDirReportsUnresolvablePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveConfigDir(""); err == nil {
		t.Error("当前目录不可用时 resolveConfigDir 应报错")
	}
}

// TestAuditServerDeprecatedInstancesOnly 断言配置里只有遗留 instances 条目（没有
// gitea 通道）时，doctor 的服务端体检仍能凭 --repo 找到目标站点。
//
// instances 是已废弃的写法但配置仍会被载入并迁移；若体检把这类配置当成「没有平台」，
// 老用户升级后会看到自己的站点凭空消失，而问题只在体检的取值路径上。
func TestAuditServerDeprecatedInstancesOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))

	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, &instances.File{
		Instances: []instances.Instance{{
			Host:     "https://legacy.example.com",
			Reviewer: instances.Account{Name: "ai"},
			Merger:   instances.Account{Name: "merge"},
			Repos:    []instances.Repo{{Name: "acme/video"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {MainAgent: "main"}},
	}); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.Flags().String("config", configPath, "")
	command.Flags().String("repo", "acme/video", "")

	// 无令牌 → 该实例的体检会回 SKIPPED，但目标站点必须被找到（否则 host 为空、
	// 结论会退化成「配置中没有平台」）
	findings, host, err := auditServer(command, configPath, &repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("遗留 instances 配置下体检不该报错：%v", err)
	}
	if !strings.Contains(host, "legacy.example.com") {
		t.Errorf("应从遗留 instances 条目找到站点，got host=%q findings=%+v", host, findings)
	}
}

// TestChannelInstanceFromDeprecatedInstance 断言 giteaViews 把遗留 instances 条目
// 折算成与 gitea 通道一致的视图（host/reviewer/merger/repos 一个不少）。
//
// doctor 与调度只读 giteaViews；折算漏字段会让老配置在体检里显示成「没有 reviewer」，
// 而配置里其实写着——这类假阴性最难自查。
func TestChannelInstanceFromDeprecatedInstance(t *testing.T) {
	file := &instances.File{
		Instances: []instances.Instance{{
			Host:     "https://legacy.example.com",
			Reviewer: instances.Account{Name: "ai"},
			Merger:   instances.Account{Name: "merge"},
			Repos:    []instances.Repo{{Name: "acme/video"}},
		}},
	}
	views := giteaViews(file)
	if len(views) != 1 {
		t.Fatalf("应折算 1 个视图，got %d", len(views))
	}
	view := views[0]
	if !sameHost(view.Host, "https://legacy.example.com") {
		t.Errorf("Host = %q", view.Host)
	}
	if view.Reviewer.Name != "ai" || view.Merger.Name != "merge" {
		t.Errorf("身份应保留：reviewer=%q merger=%q", view.Reviewer.Name, view.Merger.Name)
	}
	if len(view.Repos) != 1 || view.Repos[0].Name != "acme/video" {
		t.Errorf("仓库应保留：%+v", view.Repos)
	}
}

// TestCredentialForCoversStoreStates 断言 credentialFor 在三类凭据库状态下都给出
// 明确结论：有令牌、无该用途令牌、库损坏。
//
// 它是所有「这台机器上有没有某用途令牌」判断的唯一来源；三种状态若混在一起
// （比如把损坏当没有），操作者会被引去做一次注定无效的重新登录。
func TestCredentialForCoversStoreStates(t *testing.T) {
	host := "https://cf.example.com"
	dir := t.TempDir()
	storePath := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)

	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: host, User: "ge"})
	store.SetCredential(credentials.Credential{
		Host: host, User: "ge", Purpose: credentials.PurposeAdmin, Token: "admin-token",
	})
	if err := credentials.Save(storePath, store); err != nil {
		t.Fatal(err)
	}

	credential, ok, err := credentialFor("", host, credentials.PurposeAdmin)
	if err != nil || !ok {
		t.Fatalf("有令牌时应命中：(ok=%v, err=%v)", ok, err)
	}
	if credential.Token != "admin-token" {
		t.Errorf("令牌 = %q", credential.Token)
	}

	if _, ok, err := credentialFor("", host, credentials.PurposeMCP); err != nil || ok {
		t.Errorf("无该用途令牌应回 ok=false 且不报错：(ok=%v, err=%v)", ok, err)
	}

	if err := os.WriteFile(storePath, []byte("{ 坏 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := credentialFor("", host, credentials.PurposeAdmin); err == nil || ok {
		t.Errorf("库损坏应报错且不声称命中：(ok=%v, err=%v)", ok, err)
	}
}
