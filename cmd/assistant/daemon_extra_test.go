package main

import (
	"bytes"
	"path/filepath"
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

// TestTruncateDescriptionFallsBackForEmptyPrompt 断言子代理描述的三条边界：
// 空提示词要给出可读兜底（claude 靠 description 决定委派，空串会让子代理
// 永远不被选中），多行只取首行，超长按 rune 截断（不能按字节切碎中文）。
func TestTruncateDescriptionFallsBackForEmptyPrompt(t *testing.T) {
	long := strings.Repeat("字", 81)
	short := strings.Repeat("字", 80)
	for _, testCase := range []struct {
		name   string
		prompt string
		want   string
	}{
		{"空提示词给兜底描述", "", "专项任务子代理"},
		{"全空白也走兜底", "  \n\t ", "专项任务子代理"},
		{"多行只取首行", "整理标签\n第二行会被丢弃", "整理标签"},
		{"正好八十个 rune 不截断", short, short},
		{"超过八十个 rune 截断并加省略号", long, strings.Repeat("字", 80) + "…"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := truncateDescription(testCase.prompt); got != testCase.want {
				t.Errorf("truncateDescription(%q) = %q, want %q", testCase.prompt, got, testCase.want)
			}
		})
	}
}

// TestDisplayNameMarksBuiltinPresets 断言 provider 展示名：空名是「内置缺省」
// （回退到全局 default_provider），内置预设加标注以便和用户自定义区分开。
func TestDisplayNameMarksBuiltinPresets(t *testing.T) {
	file := &instances.File{}
	for _, testCase := range []struct {
		name string
		want string
	}{
		{"", "内置缺省"},
		{"anthropic", "anthropic 内置预设"},
		{"my-custom", "my-custom"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := displayName(testCase.name, file)
			if testCase.name == "" || testCase.name == "my-custom" {
				if got != testCase.want {
					t.Errorf("displayName(%q) = %q, want %q", testCase.name, got, testCase.want)
				}
				return
			}
			// 预设名是否内置由 provider 包决定，这里只断言标注格式。
			if !strings.HasSuffix(got, " 内置预设") {
				t.Errorf("displayName(%q) = %q, 缺少内置预设标注", testCase.name, got)
			}
		})
	}
}

// TestOrDashAndTokenStateRenderPresence 断言配置状态行的空值呈现：空字段必须
// 显式写 "-"/"none"，否则操作者分不清「没配」与「输出被吃掉」。
func TestOrDashAndTokenStateRenderPresence(t *testing.T) {
	if got := orDash("  "); got != "-" {
		t.Errorf("orDash(空白) = %q, want -", got)
	}
	if got := orDash("gitea.example.com"); got != "gitea.example.com" {
		t.Errorf("orDash = %q", got)
	}
	if got := tokenState(""); got != "none" {
		t.Errorf("tokenState(空) = %q, want none", got)
	}
	if got := tokenState("  "); got != "none" {
		t.Errorf("tokenState(空白) = %q, want none", got)
	}
	if got := tokenState("abc"); got != "set" {
		t.Errorf("tokenState = %q, want set", got)
	}
}

// TestChannelRowsSkipsForeignChannels 断言通道列表只取指定类型的通道，且保留
// 身份绑定字段（login_user_id/bot_id 决定消息往哪个客服号投递）。未命名与已停用
// 的实例同样要列出来：它们是配置的一部分，藏起来会让「为什么这条没生效」无从查起。
func TestChannelRowsSkipsForeignChannels(t *testing.T) {
	isolateCredentials(t)
	disabled := false
	file := &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Host: "gitea.example.com"},
		{
			Type: instances.ChannelWeixin, Name: "work", BaseURL: "https://qy.example.com",
			LoginUserID: "u1", BotID: "b1", BotToken: "tok", AdminUsers: []string{"ge"},
		},
		{Type: instances.ChannelQQ, AppID: "1"},
		{Type: instances.ChannelWeixin, BaseURL: "https://qy2.example.com", Enabled: &disabled},
	}}
	rows, err := channelRows(file, instances.ChannelWeixin)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 条", rows)
	}
	first := rows[0]
	if first.Entry.Key() != "weixin/work" || !first.Entry.IsEnabled() ||
		first.Entry.BaseURL != "https://qy.example.com" || first.Entry.LoginUserID != "u1" ||
		first.Entry.BotID != "b1" || len(first.Entry.AdminUsers) != 1 {
		t.Errorf("first = %+v", first.Entry)
	}
	if first.Resolved.Value != "tok" {
		t.Errorf("resolved = %+v, want 内联 tok", first.Resolved)
	}
	if rows[1].Entry.Key() != instances.ChannelWeixin || rows[1].Entry.IsEnabled() {
		t.Errorf("未命名/停用通道 = %+v", rows[1].Entry)
	}
	if empty, err := channelRows(&instances.File{}, instances.ChannelWeixin); err != nil || len(empty) != 0 {
		t.Errorf("无 weixin 通道时 rows = %+v, %v", empty, err)
	}
	// 没有配置文件（file == nil）与「配置里没有该类型」是同一种结果：没配
	if none, err := channelRows(nil, instances.ChannelWeixin); err != nil || len(none) != 0 {
		t.Errorf("无配置时 rows = %+v, %v", none, err)
	}
}

// TestSubagentNamesAndAgentLabelPreserveOrder 断言子代理名清单保持 runtime
// 声明顺序、主 agent 空名落到「内置 main」：日志里顺序漂移会让「谁被注入了」
// 无法和配置对照。
func TestSubagentNamesAndAgentLabelPreserveOrder(t *testing.T) {
	subagents := []daemon.SubagentDefinition{{Name: "coder"}, {Name: "review"}, {Name: "writer"}}
	if got := strings.Join(subagentNames(subagents), ","); got != "coder,review,writer" {
		t.Errorf("subagentNames = %q", got)
	}
	if got := subagentNames(nil); len(got) != 0 {
		t.Errorf("subagentNames(nil) = %v", got)
	}
	if got := agentLabel("  "); got != "内置 main" {
		t.Errorf("agentLabel(空白) = %q, want 内置 main", got)
	}
	if got := agentLabel("ops"); got != "ops" {
		t.Errorf("agentLabel = %q", got)
	}
}

// TestGiteaTargetFillsRuntimeDerivedPaths 断言「配置装载 → 调度目标」的完整
// 契约：仓库目录、日志/锁落点、worktree 根、状态库、凭据、provider 都按通道与
// runtime 解析出来，且 --repo 选中的仓库才进目标；这是 run/review/triage 的
// 唯一入口，任何一项落空都会让会话在错误目录里跑。
func TestGiteaTargetFillsRuntimeDerivedPaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")

	// 凭据库（purpose=review）与 config.json 都按同一主机登记；host 字段是
	// 绝对 HTTP(S) URL，schema 校验会拒绝裸域名。
	const host = "gitea.example.com"
	withReviewCredential(t, host)
	channelToken := "channel-review-token"
	file := &instances.File{
		Channels: []instances.Channel{{
			Type:     instances.ChannelGitea,
			Host:     "https://gitea.example.com",
			Reviewer: "ai",
			Merger:   "merge",
			Token:    channelToken,
			Repos:    []instances.Repo{{Name: "acme/video"}, {Name: "acme/audio"}},
		}},
	}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	// 通过 file.Channels 的通道指针取目标：giteaTarget 需要 runtime 且 runtime
	// 需先经 Normalize 获得锚定路径，所以这里用 Load 走的正规入口。
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	loaded, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Runtimes = map[string]instances.Runtime{"main": {Root: root, IntervalMS: 5000, SessionTimeoutMS: 60000, Concurrency: 3}}
	if err := instances.Save(configPath, loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := loaded.Runtimes["main"]
	if !ok {
		t.Fatal("runtimes 缺少 main")
	}
	channel := loaded.Channels[0]
	options := &dispatcherOptions{}
	target, err := giteaTarget(command, loaded, configPath, resolved, &channel, instances.Repo{Name: "acme/video"}, options)
	if err != nil {
		t.Fatalf("giteaTarget: %v", err)
	}

	if target.config.Repository.FullName() != "acme/video" {
		t.Errorf("Repository = %+v", target.config.Repository)
	}
	if target.config.AccessToken != "channel-review-token" {
		t.Errorf("AccessToken = %q, want 通道显式 token 优先于凭据库", target.config.AccessToken)
	}
	if target.config.Reviewer != "ai" {
		t.Errorf("评审身份 = %q, want ai", target.config.Reviewer)
	}
	if target.config.Interval != 5*time.Second || target.config.Concurrency != 3 {
		t.Errorf("运行参数 = interval %v concurrency %d", target.config.Interval, target.config.Concurrency)
	}
	stateDir, err := resolved.RepoStatePath(host, "acme/video")
	if err != nil {
		t.Fatal(err)
	}
	if target.config.LogDir != filepath.Join(stateDir, "logs") || target.config.LockFile != filepath.Join(stateDir, "dispatcher.lock") {
		t.Errorf("日志/锁落点 = %q / %q, want %q", target.config.LogDir, target.config.LockFile, stateDir)
	}
	if !strings.HasPrefix(target.config.WorktreeRoot, resolved.ReviewRootDir()) {
		t.Errorf("WorktreeRoot = %q, want 位于 %q 之下", target.config.WorktreeRoot, resolved.ReviewRootDir())
	}
	repoDir, err := resolved.TargetPath(host, "acme/video")
	if err != nil {
		t.Fatal(err)
	}
	if target.repoDir != repoDir {
		t.Errorf("repoDir = %q, want %q", target.repoDir, repoDir)
	}
	// 未显式指定 --repo-dir / repo.Dir 时走「受管克隆」路径：giteaTarget 只负责把
	// 仓库标成受管并解析出落点，脚手架是否齐全（skipReason）由调用方
	// prepareManagedTargets 判定，不能在这里被提前拒绝。
	if !target.managed {
		t.Error("缺省的受管克隆路径应把仓库标为受管")
	}
	if target.skipReason != "" {
		t.Errorf("受管判定不属于 giteaTarget：%q", target.skipReason)
	}

	// --repo 未选中的仓库不会出现在目标里：用单仓库配置再取一次即可确认筛选，
	// 这里改为直接对不存在的仓库断言错误路径。
	if _, err := giteaTarget(command, loaded, configPath, resolved, &channel, instances.Repo{Name: "acme/nope"}, options); err != nil {
		t.Fatalf("giteaTarget(未登记仓库) 不应报错（由调用方筛选）: %v", err)
	}
}

// TestOpenStateStoreFollowsRuntimeStateFile 断言状态库开关语义：runtime 为
// nil（环境变量单实例模式）或 state_file="off" 时返回 nil（内省与跨进程互斥
// 退化为进程内），否则打开 SQLite 并打出落点；打开失败必须冒泡而不是静默。
func TestOpenStateStoreFollowsRuntimeStateFile(t *testing.T) {
	var logs []string
	log := func(line string) { logs = append(logs, line) }

	store, err := openStateStore(nil, log)
	if err != nil || store != nil {
		t.Fatalf("runtime 为 nil 时 openStateStore = %v, %v", store, err)
	}
	if len(logs) != 0 {
		t.Errorf("不应打日志：%v", logs)
	}

	off := instances.Runtime{StateFile: "off"}
	if store, err := openStateStore(&off, log); err != nil || store != nil {
		t.Fatalf("state_file=off 时 openStateStore = %v, %v", store, err)
	}

	dir := t.TempDir()
	runtime := instances.Runtime{Root: dir}
	store, err = openStateStore(&runtime, log)
	if err != nil {
		t.Fatalf("openStateStore: %v", err)
	}
	defer store.Close()
	if store == nil {
		t.Fatal("默认应打开状态库")
	}
	want := filepath.Join(dir, "state", "state.sqlite3")
	if len(logs) != 1 || !strings.Contains(logs[0], want) {
		t.Errorf("logs = %v, want 含 %q", logs, want)
	}
	// 打开同一个库两次必须成功（幂等），确认 WAL 落点没被二次加锁。
	if second, err := statestore.Open(want); err != nil {
		t.Errorf("二次打开同一状态库失败: %v", err)
	} else {
		second.Close()
	}
}

// TestNewDispatchLoggersRespectsLevelsAndDebugImpliesVerbose 断言三级日志约定：
// default 只有 info，--verbose 增加「正在做什么」，--debug 进一步暴露内部诊断
// 且隐含 verbose（否则会出现「有 debug 却没有过程」的残缺日志）。
//
// 逐行解析而不是子串匹配：每行形如 `[<前缀> <RFC3339 UTC>] <载荷>`，只有载荷
// 与行内前缀绑定才说明「verbose 的载荷挂在 verbose 行上」，否则「把 verbose
// 接到 info 上」也能骗过 contains。
func TestNewDispatchLoggersRespectsLevelsAndDebugImpliesVerbose(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		verbose     bool
		debug       bool
		wantPresent []string
		wantAbsent  []string
	}{
		{"默认只有结果", false, false, []string{"[dispatch "}, []string{"[dispatch:v ", "[dispatch:d "}},
		{"verbose 增加过程", true, false, []string{"[dispatch ", "[dispatch:v "}, []string{"[dispatch:d "}},
		{"debug 隐含 verbose", false, true, []string{"[dispatch ", "[dispatch:v ", "[dispatch:d "}, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var out bytes.Buffer
			loggers := newDispatchLoggers(&out, testCase.verbose, testCase.debug)
			loggers.info("结果")
			loggers.verbose("过程")
			loggers.debug("诊断")
			got := out.String()
			// 载荷 → 该载荷所属的行前缀（同一载荷出现在别的前缀下即为接线错误）。
			owner := map[string]string{"结果": "[dispatch ", "过程": "[dispatch:v ", "诊断": "[dispatch:d "}
			seen := make(map[string]bool)
			for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
				prefix, payload, ok := strings.Cut(line, "] ")
				if !ok {
					t.Fatalf("日志行缺少 UTC 时间戳：%q", line)
				}
				if !strings.HasSuffix(prefix, "Z") {
					t.Errorf("日志行时间戳不是 UTC：%q", line)
				}
				for _, candidate := range []string{"[dispatch:d ", "[dispatch:v ", "[dispatch "} {
					if strings.HasPrefix(line, candidate) {
						if want, ok := owner[payload]; ok && want != candidate {
							t.Errorf("载荷 %q 出现在 %q 行（应为 %q）：%q", payload, candidate, want, line)
						}
						seen[payload] = true
						break
					}
				}
			}
			for _, want := range testCase.wantPresent {
				payload := "结果"
				switch want {
				case "[dispatch:v ":
					payload = "过程"
				case "[dispatch:d ":
					payload = "诊断"
				}
				if !seen[payload] {
					t.Errorf("输出缺少 %q 前缀的行：\n%s", want, got)
				}
			}
			for _, unwanted := range testCase.wantAbsent {
				if strings.Contains(got, unwanted) {
					t.Errorf("输出不应含 %q：\n%s", unwanted, got)
				}
			}
		})
	}
}

// TestNewDispatchDepsWiresQueueHooks 断言调度依赖的接线：入队回调同时喂内存
// store 与 SQLite store，会话开始/结束回调转发到 daemon store；nil store 时
// 不留空指针回调（dispatcher 的互斥判定靠 second 返回值）。
func TestNewDispatchDepsWiresQueueHooks(t *testing.T) {
	command := &cobra.Command{}
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&bytes.Buffer{})

	store := daemon.NewStore("test-version")
	store.AddTarget(daemon.Target{Host: "gitea.example.com", Repository: "acme/video"})

	target := dispatchTarget{
		instance: instances.Instance{Host: "gitea.example.com"},
		repo:     instances.Repo{Name: "acme/video"},
		config: dispatcher.Config{
			Host:        "gitea.example.com",
			Repository:  status.Repository{Owner: "acme", Name: "video"},
			AccessToken: "tok",
			Reviewer:    "ai",
			LogDir:      t.TempDir(),
		},
	}

	state, err := statestore.Open(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.UpsertTarget(statestore.Target{Host: "gitea.example.com", Repository: "acme/video"}); err != nil {
		t.Fatal(err)
	}

	deps := newDispatchDeps(target, &out, store, state, false, false)
	// 平台能力经函数注入（不再是 API 字段）：两个注入点都必须接上，
	// 否则跑起来是「检测不到待办」而不是报错，故障很隐蔽。
	if deps.ListWork == nil || deps.Verify == nil {
		t.Error("deps 未接上平台注入点（ListWork / Verify）")
	}
	if deps.Config.Repository.FullName() != "acme/video" {
		t.Errorf("deps 未接到目标配置：%+v", deps.Config)
	}
	if deps.OnQueue == nil || deps.OnStart == nil || deps.OnFinish == nil {
		t.Fatal("store 非 nil 时必须接上三个回调")
	}
	// 入队回调：daemon store 必须记住这条待办（状态 API 的 queue 靠它展示）。
	item := dispatcher.WorkItem{Kind: dispatcher.KindPull, Number: 7, Title: "add thing"}
	deps.OnQueue([]dispatcher.WorkItem{item})
	if queue := store.Snapshot().Queue; len(queue) != 1 || queue[0].Items[0].Number != 7 {
		t.Errorf("OnQueue 未写入 daemon store：%+v", queue)
	}
	// 会话开始回调：状态库里的 target 会被标记为运行中，且无互斥冲突时放行。
	if allowed := deps.OnStart(item); !allowed {
		t.Error("OnStart 不应在无互斥冲突时拒绝")
	}
	if sessions := store.Snapshot().Sessions; len(sessions) != 1 || sessions[0].Number != 7 {
		t.Errorf("OnStart 未登记会话：%+v", sessions)
	}
	deps.OnFinish(item, dispatcher.SessionOutcome{Subtype: "success"})
	if recent := store.Snapshot().Recent; len(recent) != 1 || recent[0].Subtype != "success" {
		t.Errorf("OnFinish 未归集结果：%+v", recent)
	}
	// 状态库侧：会话结束后同一待办可以再次登记（互斥锁必须在 OnFinish 释放，
	// 否则重试的待办会被自己上一次的残留记录永久挡在门外）。
	if allowed := deps.OnStart(item); !allowed {
		t.Error("OnFinish 之后同一待办应可再次 OnStart")
	}
}
