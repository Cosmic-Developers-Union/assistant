package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/daemon"
	"github.com/Cosmic-Developers-Union/assistant/internal/dispatcher"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/statestore"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

// initGitCheckout 造一个真实的最小 git 检出：remote.go 的 RepoRoot/ListRemotes
// 都是拿真 git 命令问出来的，用假目录只能测到「探测失败」这一条分支，测不出
// resolveEnvDispatcher 从子目录回溯到仓库根、以及按 remote 判定站点这两条
// 关键语义。
func initGitCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	return dir
}

// TestResolveEnvDispatcherUsesRepoRootAndEnvironment 断言环境变量单实例模式的
// 定位语义：从仓库子目录调用时 repoDir 必须回溯到仓库根（评审要在根上跑
// fetch/worktree，落在子目录等于把 .git 之外的东西当成仓库）；host/repo/token
// 三项来自 GITEA_* 且去掉首尾空白（令牌带换行是 .env 抄写最常见的形态，原样
// 传下去会 401）。这是「配置文件可选」承诺的另一半：没有 config.json 时也要
// 能靠环境变量把身份给全。
func TestResolveEnvDispatcherUsesRepoRootAndEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initGitCheckout(t)
	runGit(t, root, "remote", "add", "origin", "https://gitea.example.com/acme/rocket.git")
	nested := filepath.Join(root, "internal", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	t.Setenv("GITEA_HOST", "  https://gitea.example.com  ")
	t.Setenv("GITEA_REPOSITORY", " acme/rocket ")
	t.Setenv("GITEA_ACCESS_TOKEN", " env-token \n")

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	config, repoDir, err := resolveEnvDispatcher(command, "", &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveEnvDispatcher: %v", err)
	}
	if config.Host != "https://gitea.example.com" || config.AccessToken != "env-token" {
		t.Errorf("host/token = %q / %q, want 去空白后的环境变量值", config.Host, config.AccessToken)
	}
	if config.Repository.FullName() != "acme/rocket" {
		t.Errorf("repository = %q, want acme/rocket", config.Repository.FullName())
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantRoot {
		t.Errorf("repoDir = %q, want 回溯到仓库根 %q", got, wantRoot)
	}
}

// TestResolveEnvDispatcherPrefersExplicitRepoDir 断言 --repo-dir 在环境变量模式
// 下的优先级与失败姿态：显式给了就绝不猜当前目录（--repo-dir 是「这个目录里的
// 会话就是我要的」，猜错等于在别的检出上评审）；而检出既没有可用 remote 又没给
// --host/GITEA_HOST 时必须硬失败——站点身份缺失还继续跑，会拿空 host 去建
// status.Client。
func TestResolveEnvDispatcherPrefersExplicitRepoDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	explicit := t.TempDir()
	t.Setenv("GITEA_HOST", "https://gitea.example.com")
	t.Setenv("GITEA_REPOSITORY", "acme/rocket")
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	_, repoDir, err := resolveEnvDispatcher(command, "", &dispatcherOptions{RepoDir: explicit})
	if err != nil {
		t.Fatalf("resolveEnvDispatcher: %v", err)
	}
	if repoDir != explicit {
		t.Errorf("repoDir = %q, want 显式 --repo-dir %q", repoDir, explicit)
	}

	// 没有 remote 的检出 + 没有站点来源：必须报缺少站点，而不是回落到默认站点。
	t.Setenv("GITEA_HOST", "")
	runGit(t, explicit, "init", "-q")
	t.Chdir(explicit)
	if _, _, err := resolveEnvDispatcher(command, "", &dispatcherOptions{}); err == nil ||
		!strings.Contains(err.Error(), "缺少 Gitea 站点") {
		t.Fatalf("err = %v, want 缺少 Gitea 站点", err)
	}
}

// TestResolveEnvDispatcherIgnoresNonGiteaRemote 断言「origin 指向 GitHub 这类
// 非 Gitea 站点」时不把 remote 当站点用：探测失败且没给 --host/GITEA_HOST 时
// 必须报缺少站点。误把 GitHub remote 当 Gitea，doctor/run 会拿着 Gitea 令牌去
// 打 api.github.com，得到的是 404 而不是「你没配站点」。
func TestResolveEnvDispatcherIgnoresNonGiteaRemote(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initGitCheckout(t)
	runGit(t, root, "remote", "add", "origin", "https://github.com/acme/rocket.git")
	t.Chdir(root)
	t.Setenv("GITEA_HOST", "")
	t.Setenv("GITEA_REPOSITORY", "acme/rocket")
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if _, _, err := resolveEnvDispatcher(command, "", &dispatcherOptions{}); err == nil ||
		!strings.Contains(err.Error(), "缺少 Gitea 站点") {
		t.Fatalf("err = %v, want 缺少 Gitea 站点（非 Gitea remote 不算站点）", err)
	}

	// 显式 --host 时同一个检出是合法的：remote 只是自动检测的兜底，不是否决。
	_, repoDir, err := resolveEnvDispatcher(command, "", &dispatcherOptions{Host: "https://gitea.example.com"})
	if err != nil {
		t.Fatalf("显式 --host 时应放行: %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantRoot {
		t.Errorf("repoDir = %q, want %q", got, wantRoot)
	}
}

// TestResolveDispatchTargetsRejectsReposLessChannels 断言「有 gitea 通道但没登记
// 仓库、且没有任何可用的对话通道」是错误而不是空跑：这种配置下没有仓库可调度、
// 也没有对话服务可兜底，静默产出零目标会让 run 常驻却什么都不做（还占着锁），
// 报错要求先 assistant setup 才是可行动的结论。
func TestResolveDispatchTargetsRejectsReposLessChannels(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai",
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: data}},
	}); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)

	_, err := resolveDispatchTargets(command, "", configPath, &dispatcherOptions{})
	if err == nil || !strings.Contains(err.Error(), "没有可运行的仓库") || !strings.Contains(err.Error(), "assistant setup") {
		t.Fatalf("err = %v, want 没有可运行的仓库 + setup 指引", err)
	}
	// 半配置状态仍要在 stderr 点名是哪条通道缺仓库，否则操作者不知道去 setup 谁。
	if !strings.Contains(stderr.String(), "未配置仓库") {
		t.Errorf("未点名空通道：%q", stderr.String())
	}
}

// TestPrepareManagedTargetsSkipsOccupiedCloneAndKeepsOthers 断言受管克隆落点被
// 非 git 目录占用时的隔离语义：EnsureRepo 会拒绝落子，该仓库必须被标记
// skipReason 并在 stderr 点名跳过，而同一轮里的其它仓库照常继续。把整轮
// 拖垮等于「一个坏目录让所有仓库停摆」，而静默跳过则会让操作者以为它还在跑。
func TestPrepareManagedTargetsSkipsOccupiedCloneAndKeepsOthers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	fake := newFakeGitea(t)
	writeFakeGiteaConfig(t, fake, "acme/rocket", "acme/lab")

	// 邻居走「真 clone 成功」这条路：EnsureRepo 只在落点为空的目录里才克隆，
	// 落点指向假站点只会得到「邻居也失败」，测不出「一个坏落点不拖累同一轮的
	// 其它仓库」这条隔离语义。
	//
	// 克隆 URL 由实例地址折算（`<host 去尾斜杠>/<owner>/<repo>.git`），落点则由
	// 目标自己的 repoDir 决定，两者互不相干：手工指定 repoDir 就不必依赖 runtime
	// 的 slug 折算，也不会把落点落进裸仓库里面（落点非空即不克隆，落在裸仓库里
	// 必然失败）。这里把裸仓库放在独立的一级目录下，让实例地址指向它，克隆于是
	// 走本地路径；待克隆的落点在同级的另一个目录，两侧都不在对方里面。
	sandbox := t.TempDir()
	bare := filepath.Join(sandbox, "srv", "acme", "lab.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	// 落点必须在调用前就存在：EnsureRepo 只创建落点的上一级，不会创建落点
	// 本身，而 git clone 允许克隆进已存在的空目录。
	labDir := filepath.Join(sandbox, "clone", "lab")
	if err := os.MkdirAll(labDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 被占用的落点：非空且不是 git 检出，EnsureRepo 必须拒绝落子。
	occupied := filepath.Join(sandbox, "occupied", "rocket")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)

	// 两个仓库都用显式落点（managed=false 分支），落点不进 runtime 布局，测试
	// 也就不依赖 HostSlug 的折算；受管这条路径由下面手工打开。
	file := &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: fake.server.URL, Reviewer: "ai",
			Token: "$GITEA_ACCESS_TOKEN",
			Repos: []instances.Repo{
				{Name: "acme/rocket", Dir: occupied},
				{Name: "acme/lab", Dir: labDir},
			},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: t.TempDir()}},
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("GITEA_ACCESS_TOKEN", "reviewer-token")
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	runtime := file.Runtimes["main"]
	channel := &file.Channels[0]
	options := &dispatcherOptions{}

	rocket, err := giteaTarget(command, file, configPath, runtime, channel, channel.Repos[0], options)
	if err != nil {
		t.Fatalf("折算 acme/rocket: %v", err)
	}
	neighbor, err := giteaTarget(command, file, configPath, runtime, channel, channel.Repos[1], options)
	if err != nil {
		t.Fatalf("折算 acme/lab: %v", err)
	}
	// repo.Dir 显式落点走的是「共享检出」分支（managed=false），这里手工打开受管：
	// 本测试要测的正是受管克隆这条路径。邻居的实例地址换成装裸仓库的那一级，
	// 克隆 URL 于是是 `<该级>/acme/lab.git`，命中 sandbox/srv 下的本地裸仓库。
	rocket.managed, neighbor.managed = true, true
	neighbor.instance = instances.Instance{Host: filepath.Join(sandbox, "srv")}

	targets := []dispatchTarget{rocket, neighbor}
	got := prepareManagedTargets(command, targets)
	if len(got) != 2 {
		t.Fatalf("targets = %d, want 2（跳过的目标仍留在清单里供上层汇总）", len(got))
	}
	if got[0].skipReason == "" || !strings.Contains(got[0].skipReason, "已被占用") {
		t.Errorf("占用的落点 skipReason = %q, want 落点被占用", got[0].skipReason)
	}
	if !strings.Contains(stderr.String(), "跳过 acme/rocket") {
		t.Errorf("未在 stderr 点名跳过：%q", stderr.String())
	}
	// 同一轮的另一个仓库必须照常克隆成功，不受邻居拖累。
	if got[1].skipReason != "" {
		t.Errorf("邻居 skipReason = %q, want 空", got[1].skipReason)
	}
	if !strings.Contains(stderr.String(), "已克隆 acme/lab") {
		t.Errorf("未报告邻居克隆成功：%q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(got[1].repoDir, ".git")); err != nil {
		t.Errorf("邻居落点应已是 git 检出：%v", err)
	}
	if ready := readyTargets(got); len(ready) != 1 || ready[0].repo.Name != "acme/lab" {
		t.Errorf("readyTargets = %+v, want 只有 acme/lab", ready)
	}
}

// TestPrepareManagedTargetsLeavesUnmanagedAlone 断言非受管目标（--repo-dir 指定的
// 既有检出）绝不被 prepare 碰：它一个 git 命令都不该发。托管检出是 assistant
// 的财产，宿主自己的检出不是——在用户的检出上跑 EnsureRepo 会往人家的目录里
// 克隆，或者把「非 git 目录」判成错误。
func TestPrepareManagedTargetsLeavesUnmanagedAlone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)

	targets := []dispatchTarget{{
		repo:    instances.Repo{Name: "acme/rocket"},
		config:  dispatcher.Config{Host: "https://gitea.example.com", AccessToken: "t"},
		repoDir: dir,
		managed: false,
	}}
	got := prepareManagedTargets(command, targets)
	if len(got) != 1 || got[0].skipReason != "" {
		t.Fatalf("非受管目标 = %+v, want 原样返回", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("非受管目标不应产生任何输出：%q", stderr.String())
	}
	if !strings.Contains(filepath.Join(dir, "plain.txt"), got[0].repoDir) {
		t.Errorf("repoDir = %q, want 原样保留 %q", got[0].repoDir, dir)
	}
}

// commentsPath 匹配 /repos/{owner}/{repo}/issues/{n}/comments：假 Gitea 的
// 通用分支对未知资源回的是数组，而写评论端点回单对象，混用会让
// CreateIssueComment 报「cannot unmarshal array into ... Comment」——测出来的是
// 假站点不真，而不是被测代码有错。
func commentsPath(path string) bool {
	if !strings.HasPrefix(path, "/repos/") {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
	// 真站点（以及 SDK 的 CreateIssueComment）用的是
	// /repos/{owner}/{repo}/issues/{n}/comments：owner、repo 之后还有 issues 与
	// 编号两段，"comments" 落在第 5 段（下标 4），不是第 4 段。
	return len(segments) >= 5 && segments[2] == "issues" && segments[4] == "comments"
}

// wrapFakeGiteaComments 在已有处理器外面套一层：写评论返回单个 Comment 对象
// （与真站点一致），其余请求原样交给内层处理器。
func wrapFakeGiteaComments(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if commentsPath(path) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "body": "ok", "user": map[string]any{"login": "ai"},
			})
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// TestNewDispatchDepsPinsPromptAndFollowUp 断言 deps 的两条契约：提示词必须带上
// config.Repository 的全名（少了它，评审会话不知道自己在审哪个仓库，gitea MCP
// 也只能靠猜）；CurrentLogin 走的是 reviewer 令牌的真实身份（假装成别人会导致
// review 落错账号）；FollowUpMessages 只取会话开始之后、非 assistant 账号、且
// 正文非空的新评论——把 reviewer 自己的评论也交回去会让会话陷入自问自答。
func TestNewDispatchDepsPinsPromptAndFollowUp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	target, _ := fakeGiteaCommand(t, fake, "")

	var logged bytes.Buffer
	deps := newDispatchDeps(target, &logged, nil, nil, false, false)

	// BuildPrompt 返回单值（内部已锚定仓库全名），extra.Repository 由 deps 覆盖。
	prompt := deps.BuildPrompt(dispatcher.KindPull, 7, dispatcher.PromptContext{Repository: "被覆盖的占位"})
	if !strings.Contains(prompt, "acme/rocket") {
		t.Errorf("提示词缺少仓库全名：\n%s", truncateForTest(prompt))
	}
	if strings.Contains(prompt, "被覆盖的占位") {
		t.Errorf("调用方给的 Repository 应被 deps 覆盖：\n%s", truncateForTest(prompt))
	}

	login, err := deps.CurrentLogin(t.Context())
	if err != nil {
		t.Fatalf("CurrentLogin: %v", err)
	}
	if login != "ai" {
		t.Errorf("login = %q, want ai（令牌对应账号）", login)
	}

	// 假站点没有该 PR 的评论：结果是空清单且无错，说明过滤逻辑走的是真实
	// client（而不是凭空造数据）。
	messages, err := deps.FollowUpMessages(t.Context(),
		dispatcher.WorkItem{Kind: dispatcher.KindPull, Number: 7}, time.Time{})
	if err != nil {
		t.Fatalf("FollowUpMessages: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("messages = %v, want 无评论时为空", messages)
	}

	// 非受管目标且 SyncMirror 关闭：deps.SyncMirror 必须为 nil，否则循环会去
	// 同步一个它不拥有的检出。
	if deps.SyncMirror != nil {
		t.Error("非受管且未开 sync-mirror 时 SyncMirror 应为 nil")
	}
	// 内存/状态库都为空时三个回调都必须为 nil：插件式回调为空是「不写运行态」
	// 的正常形态，非 nil 的空实现会让 dispatcher 以为有人在记录。
	if deps.OnQueue != nil || deps.OnStart != nil || deps.OnFinish != nil {
		t.Error("无 store 时 OnQueue/OnStart/OnFinish 应为 nil")
	}
	if deps.RepoDir != target.repoDir || deps.ListWork == nil || deps.Verify == nil {
		t.Error("deps 必须带上 repoDir 与平台注入点（否则会话无处可跑 / 检测不到待办）")
	}
}

// truncateForTest 把提示词截断到可读长度：失败信息里贴整段提示词会淹没其它断言。
func truncateForTest(text string) string {
	if len(text) > 400 {
		return text[:400] + "…"
	}
	return text
}

// TestNewDispatchDepsSyncMirrorForManagedTarget 断言受管目标即使没开
// --sync-mirror 也会拿到同步函数：受管克隆是 assistant 的财产，评审标准
// （.claude/）与 Issue 分诊都要求它与 origin 基线严格一致，不自动同步等于
// 拿上一轮的旧标准审新提交。同时断言 PostFollowUpNote 真的把「有新消息、
// 先读后答」写进站点评论（而不是只记一行日志）。
func TestNewDispatchDepsSyncMirrorForManagedTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.server.Config.Handler = wrapFakeGiteaComments(fake.server.Config.Handler)
	fake.login["reviewer-token"] = "ai"
	root := initGitCheckout(t)
	// 造一个真 remote 指向本地另一个检出，避免任何网络往返。
	remoteDir := initGitCheckout(t)
	runGit(t, remoteDir, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, root, "remote", "add", "origin", remoteDir)

	client, err := status.NewClient(fake.server.URL, "reviewer-token")
	if err != nil {
		t.Fatalf("status.NewClient: %v", err)
	}
	target := dispatchTarget{
		instance: instances.Instance{Host: fake.server.URL},
		repo:     instances.Repo{Name: "acme/rocket"},
		config: dispatcher.Config{
			Host: fake.server.URL, AccessToken: "reviewer-token", Reviewer: "ai", BaseBranch: "main",
			Repository: status.Repository{Owner: "acme", Name: "rocket"},
		},
		client:  client,
		repoDir: root,
		managed: true,
	}
	var out bytes.Buffer
	deps := newDispatchDeps(target, &out, nil, nil, false, false)

	if deps.SyncMirror == nil {
		t.Fatal("受管目标的 SyncMirror 不应为 nil")
	}
	sha, err := deps.SyncMirror()
	if err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	if strings.TrimSpace(sha) == "" {
		t.Error("SyncMirror 应返回基线短 sha")
	}

	// PostFollowUpNote 写站点评论：假站点接受任何 POST 并回显，所以这里钉的是
	// 「确实调用了写评论这条路径」（无错返回即代表请求成功）。
	if err := deps.PostFollowUpNote(t.Context(),
		dispatcher.WorkItem{Kind: dispatcher.KindPull, Number: 7}, 3); err != nil {
		t.Fatalf("PostFollowUpNote: %v", err)
	}
}

// TestNewDispatchDepsWiresStoreAndStateStore 断言两个运行态存储被真正接上：
// 内存 Store 记录待办与结果，状态库（SQLite）承担跨进程单飞——同一个待办第二次
// OnStart 必须返回 false（否则同一 PR 会被两个进程并发评审，两份结论互相覆盖），
// OnFinish 收尾后又要重新可开始。这是「一个待办同一时刻只有一个会话」这条
// 语义的可执行定义。
func TestNewDispatchDepsWiresStoreAndStateStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)

	store := daemon.NewStore("test")
	stateDir := t.TempDir()
	stateStore, err := statestore.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatalf("statestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close() })

	// 回调在装配时闭包捕获 instance.Host 与 repo.Name：两者都必须非空，否则
	// store.SetQueue 会以空键落库、Snapshot 里就查不到这条待办。
	const host = "https://gitea.example.com"
	const repository = "acme/rocket"
	target := dispatchTarget{
		instance: instances.Instance{Host: host},
		repo:     instances.Repo{Name: repository},
		config: dispatcher.Config{
			Host: host, AccessToken: "t", BaseBranch: "main",
			Repository: status.Repository{Owner: "acme", Name: "rocket"},
			// 状态库互斥靠「残留 running 超过 2×会话超时才被接管」判定，零值超时
			// 会让上一行的记录立刻过期、第二次 OnStart 又被放行，测不出单飞。
			SessionTimeout: time.Hour,
		},
		repoDir: t.TempDir(),
	}
	var out bytes.Buffer
	deps := newDispatchDeps(target, &out, store, stateStore, false, false)

	item := dispatcher.WorkItem{Kind: dispatcher.KindPull, Number: 7, Title: "add widget"}

	// daemon.Store 的快照只列 AddTarget 登记过的仓库键（order 决定可见性），
	// 所以这里先登记目标——运行期由 daemon 启动流程做，测试里手工补上。
	store.AddTarget(daemon.Target{Host: host, Repository: repository})

	if deps.OnQueue == nil || deps.OnStart == nil || deps.OnFinish == nil {
		t.Fatal("给了 store 之后三个回调都必须接上")
	}
	deps.OnQueue([]dispatcher.WorkItem{item})
	// 内存快照必须能读出这条待办：对话侧靠它回答「现在在忙什么」。
	snapshot := store.Snapshot()
	if len(snapshot.Queue) != 1 || len(snapshot.Queue[0].Items) != 1 || snapshot.Queue[0].Items[0].Number != 7 {
		t.Fatalf("queue = %+v, want 一条 pull#7", snapshot.Queue)
	}
	if snapshot.Queue[0].Host != host || snapshot.Queue[0].Repository != repository {
		t.Errorf("queue 归属 = %q/%q, want 装配期的 %q/%q",
			snapshot.Queue[0].Host, snapshot.Queue[0].Repository, host, repository)
	}

	if !deps.OnStart(item) {
		t.Fatal("首次 OnStart 应为 true")
	}
	if deps.OnStart(item) {
		t.Error("同一待办第二次 OnStart 应为 false（状态库单飞）")
	}
	if len(store.Snapshot().Sessions) != 1 {
		t.Errorf("sessions = %+v, want 一条进行中会话", store.Snapshot().Sessions)
	}

	deps.OnFinish(item, dispatcher.SessionOutcome{
		Subtype: "success", NumTurns: 3, CostUSD: 0.5,
	})
	if len(store.Snapshot().Sessions) != 0 {
		t.Error("OnFinish 后不应还有进行中会话")
	}
	recent := store.Snapshot().Recent
	if len(recent) != 1 || recent[0].Subtype != "success" || recent[0].NumTurns != 3 {
		t.Errorf("recent = %+v, want 一条 success 归档（含轮次与花费）", recent)
	}
	if recent[0].Host != host || recent[0].Repository != repository {
		t.Errorf("归档归属 = %q/%q, want %q/%q", recent[0].Host, recent[0].Repository, host, repository)
	}
	// 收尾后才能重新开始同一待办：堵死的单飞锁会让重试永远失败。
	if !deps.OnStart(item) {
		t.Error("OnFinish 之后应可重新开始")
	}
	deps.OnFinish(item, dispatcher.SessionOutcome{Subtype: "cleanup"})
}

// TestRunDispatchLoopStopsWhenStateStoreUnopenable 断言状态库打不开时 run 直接
// 失败而不是降级继续：状态库承担跨进程互斥（同一 PR 两个进程同时评审会互相
// 覆盖结论），静默跳过等于把单飞保证悄悄关掉。这里让 state_file 落在一个普通
// 文件下面，Open 的 MkdirAll 必然失败。
func TestRunDispatchLoopStopsWhenStateStoreUnopenable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: fake.server.URL, Reviewer: "ai",
			Token: "reviewer-token",
			Repos: []instances.Repo{{Name: "acme/rocket"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {
			Root:       t.TempDir(),
			StateFile:  filepath.Join(blocker, "state.db"),
			IntervalMS: 60000,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	withReviewCredential(t, strings.TrimPrefix(fake.server.URL, "http://"))

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())

	err := runDispatchLoop(command, "", configPath, &dispatcherOptions{})
	if err == nil {
		t.Fatal("状态库打不开时 run 必须失败，而不是关掉跨进程互斥继续跑")
	}
	if !strings.Contains(err.Error(), "状态库") {
		t.Errorf("err = %v, want 点名状态库", err)
	}
}

// TestRunDispatchOneShotSurfacesWorktreeRootAndLockFailures 断言一次性命令在
// 「工作区基名不可用」与「锁已被活跃进程持有」两种前置条件下的失败姿态：
// review/triage 以某个账号在真实 PR 上落结论，这两处静默失败意味着要么评审
// 实验写进了不该写的目录，要么两个进程在同一 PR 上同时说话。
func TestRunDispatchOneShotSurfacesWorktreeRootAndLockFailures(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	configPath := writeFakeGiteaConfig(t, fake, "acme/rocket")

	repoDir := initGitCheckout(t)
	runGit(t, repoDir, "remote", "add", "origin", fake.server.URL+"/acme/rocket.git")

	// 工作区基名指向普通文件下面：MkdirAll 必然失败。注意 os.MkdirAll 报的是
	// PathError（逐级 mkdir 后拿到 ENOTDIR），消息里带的是失败的那一级路径
	// （blocker），不是完整的工作区基名，所以下面按「指向 blocker 这一级」断言。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPATCH_WORKTREE_ROOT", filepath.Join(blocker, "worktrees"))

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)
	command.SetContext(t.Context())

	options := &dispatcherOptions{
		RepoDir:    repoDir,
		BaseBranch: "main",
		// LogDir 与 WorktreeRoot 分开存放：LogDir 必须先建成，这样才能确认失败
		// 发生在工作区这一步，而不是被日志目录的失败提前盖过去。
		LogDir:   t.TempDir(),
		LockFile: filepath.Join(t.TempDir(), "dispatcher.lock"),
	}
	err := runDispatchOneShot(command, "", configPath, options, dispatcher.KindPull, 9)
	if err == nil {
		t.Fatal("工作区基名不可用时应失败")
	}
	if !strings.Contains(err.Error(), blocker) {
		t.Errorf("err = %v, want 点名失败的那一级路径 %q", err, blocker)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want 暴露底层 ENOTDIR 原因", err)
	}
	if !strings.Contains(stderr.String(), "当前账户：@ai（reviewer=ai）") {
		t.Errorf("身份行应在失败前落盘：%q", stderr.String())
	}

	// 锁被活跃进程（PID 1）持有：必须拒绝，而不是在别人的调度循环旁边再起一份。
	lockDir := t.TempDir()
	lockFile := filepath.Join(lockDir, "dispatcher.lock")
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options.WorktreeRoot = t.TempDir()
	options.LockFile = lockFile
	err = runDispatchOneShot(command, "", configPath, options, dispatcher.KindPull, 9)
	if err == nil || !strings.Contains(err.Error(), "已在运行") {
		t.Fatalf("err = %v, want dispatcher 已在运行", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("PID %d", 1)) {
		t.Errorf("err = %v, want 点名持锁 PID", err)
	}
}

// TestResolveDispatchTargetsRejectsMultipleReposWithRepoDir 断言 --repo-dir 与
// 多仓库配置互斥：--repo-dir 表达的是「这个检出就是全部」，一旦配置里登记了
// 多个仓库，拿同一个目录去跑所有仓库等于用同一份工作区并发评审不同 PR。
func TestResolveDispatchTargetsRejectsMultipleReposWithRepoDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai",
			Repos: []instances.Repo{{Name: "acme/rocket"}, {Name: "acme/lab"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: data}},
	}); err != nil {
		t.Fatal(err)
	}
	withReviewCredential(t, "https://gitea.example.com")
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	_, err := resolveDispatchTargets(command, "", configPath, &dispatcherOptions{RepoDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "--repo-dir 只适用于单个仓库") {
		t.Fatalf("err = %v, want --repo-dir 只适用于单个仓库", err)
	}
	// 单仓库时同一个 --repo-dir 是合法的：这是「本地检出接进受管调度」的正路。
	single := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(single, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai",
			Repos: []instances.Repo{{Name: "acme/rocket"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: data}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	targets, err := resolveDispatchTargets(command, "", single, &dispatcherOptions{RepoDir: dir})
	if err != nil {
		t.Fatalf("单仓库 + --repo-dir 应放行: %v", err)
	}
	if len(targets) != 1 || targets[0].repoDir != dir {
		t.Errorf("targets = %+v, want 一条指向 --repo-dir 的目标", targets)
	}
	if targets[0].managed {
		t.Error("--repo-dir 指定的检出不是受管克隆")
	}
}

// TestGiteaTargetManagedDefaultsUseRuntimePaths 断言受管目标的三条运行期落点
// 全部跟随 runtime：克隆落在 TargetPath、状态（日志/锁）落在 RepoStatePath、
// 工作区落在 ReviewDirBase 之下。这些路径是「运行产物跟随 $root」这条约定的
// 实现，落错会让多个实例互相覆盖对方的锁与日志。
func TestGiteaTargetManagedDefaultsUseRuntimePaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")
	// 用通道级环境变量兜底令牌（不写 token 字段，也不给 purpose 凭据）：token 为
	// 空时 giteaTarget 会去凭据库找 purpose=review，找不到就直接报「缺少 review
	// 用途令牌」——那正好把这条路径钉成可回归的错误语义，而不是意外失败。
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	// 通道令牌引用 $GITEA_ACCESS_TOKEN（消费点展开），这样无需凭据库就能解析出令牌。
	t.Setenv("DISPATCH_BASE_BRANCH", "develop")

	file := &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai",
			Token: "$GITEA_ACCESS_TOKEN",
			Repos: []instances.Repo{{Name: "acme/rocket"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {
			Root: root, IntervalMS: 30000, SessionTimeoutMS: 90000, Concurrency: 3,
		}},
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	target, err := giteaTarget(command, file, "", runtime, &file.Channels[0], file.Channels[0].Repos[0], &dispatcherOptions{})
	if err != nil {
		t.Fatalf("giteaTarget: %v", err)
	}
	if !target.managed {
		t.Error("未给 --repo-dir 时 sighting 的仓库应视为受管克隆")
	}
	wantDir, err := runtime.TargetPath("https://gitea.example.com", "acme/rocket")
	if err != nil {
		t.Fatal(err)
	}
	if target.repoDir != wantDir {
		t.Errorf("repoDir = %q, want %q", target.repoDir, wantDir)
	}
	stateDir, err := runtime.RepoStatePath("https://gitea.example.com", "acme/rocket")
	if err != nil {
		t.Fatal(err)
	}
	if target.config.LogDir != filepath.Join(stateDir, "logs") {
		t.Errorf("LogDir = %q, want %q", target.config.LogDir, filepath.Join(stateDir, "logs"))
	}
	if target.config.LockFile != filepath.Join(stateDir, "dispatcher.lock") {
		t.Errorf("LockFile = %q, want %q", target.config.LockFile, filepath.Join(stateDir, "dispatcher.lock"))
	}
	if !strings.HasPrefix(target.config.WorktreeRoot, filepath.Join(root, "review")) {
		t.Errorf("WorktreeRoot = %q, want 落在 review 基名之下", target.config.WorktreeRoot)
	}
	if target.config.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want develop（runtime 覆盖默认）", target.config.BaseBranch)
	}
	if target.config.Interval.Seconds() != 30 || target.config.SessionTimeout.Seconds() != 90 || target.config.Concurrency != 3 {
		t.Errorf("interval/timeout/concurrency = %v/%v/%d, want 30s/90s/3",
			target.config.Interval, target.config.SessionTimeout, target.config.Concurrency)
	}
	if target.client == nil {
		t.Error("target 必须带 status.Client（否则健康检查无处可打）")
	}
}

// TestGiteaTargetChannelTokenExpansionFailsLoudly 断言通道令牌里的环境变量引用
// 展开失败时直接报错并点名来源字段：令牌引用写错（比如引用了不存在的变量）
// 时若继续跑，会话会以空令牌调 Gitea，表现为一堆 401 而不是一句「你的
// channels[...].token 引用了不存在的变量」。
func TestGiteaTargetChannelTokenExpansionFailsLoudly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	file := &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai",
			Token: "${ABSENT_TOKEN_VAR}",
			Repos: []instances.Repo{{Name: "acme/rocket"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: root}},
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	_, err = giteaTarget(command, file, "", runtime, &file.Channels[0], file.Channels[0].Repos[0], &dispatcherOptions{})
	if err == nil {
		t.Fatal("未定义的令牌引用应报错")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v, want 点名 token 字段", err)
	}
}

// TestGiteaTargetExplicitDirSkipsManagedState 断言 --repo-dir 与 repo.Dir 两条
// 非受管路径都不写受管状态（不钉 SyncMirror、不落 logs/lock）：宿主自己的检出
// 不该被 assistant 的调度状态污染，锁与日志属于受管克隆的财产。
func TestGiteaTargetExplicitDirSkipsManagedState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	tested := filepath.Join(t.TempDir(), "checkout")
	file := &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai", Token: "channel-token",
			Repos: []instances.Repo{{Name: "acme/rocket", Dir: tested}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: root}},
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	target, err := giteaTarget(command, file, "", runtime, &file.Channels[0], file.Channels[0].Repos[0], &dispatcherOptions{})
	if err != nil {
		t.Fatalf("giteaTarget: %v", err)
	}
	if target.managed {
		t.Error("repo.Dir 指定的检出不是受管克隆")
	}
	if target.repoDir != tested {
		t.Errorf("repoDir = %q, want %q", target.repoDir, tested)
	}
	if target.config.SyncMirror {
		t.Error("非受管检出不应自动同步（会覆盖作者的分支）")
	}
	if target.config.LogDir == "" || target.config.LockFile == "" {
		t.Error("非受管检出仍要有日志与锁路径（否则 run 无处写）")
	}
	if target.config.AccessToken != "channel-token" {
		t.Errorf("AccessToken = %q, want 通道令牌", target.config.AccessToken)
	}
}
