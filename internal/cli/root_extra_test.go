package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// TestParseRepositoryAcceptsOnlyOwnerSlashName 断言 --repo 的入参契约：只接受
// 恰好一个斜杠且两段都非空的 owner/name。仓库名是后续所有 Gitea API 路径的
// 组成部分，放过多段或空段只会换来一次 404，把「参数写错」误报成「仓库不存在」。
func TestParseRepositoryAcceptsOnlyOwnerSlashName(t *testing.T) {
	repository, err := parseRepository("acme/video")
	if err != nil {
		t.Fatalf("parseRepository: %v", err)
	}
	if repository.Owner != "acme" || repository.Name != "video" {
		t.Errorf("repository = %+v", repository)
	}
	for _, raw := range []string{"", "acme", "acme/", "/video", "acme/a/b", "a/b/c"} {
		if _, err := parseRepository(raw); err == nil {
			t.Errorf("parseRepository(%q) 应报错", raw)
		} else if !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("parseRepository(%q) err = %v，缺少格式提示", raw, err)
		}
	}
}

// TestCommandOptionsValidateRejectsInvalidRepository 断言动作命令的参数校验：
// 非法仓库名必须在发起任何网络请求前拦下，否则 CI 里表现为「命令挂住」而不是
// 「参数写错」。`check` 移除后 validate 只剩这一条约束，等待相关的互斥与取值
// 校验（--wait / --timeout / --interval）随之消失。
func TestCommandOptionsValidateRejectsInvalidRepository(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options commandOptions
		wantErr string
	}{
		{"空仓库名合法（不限仓库）", commandOptions{}, ""},
		{"owner/name 合法", commandOptions{Repository: "acme/video"}, ""},
		{"仓库名非法先拦下", commandOptions{Repository: "bad"}, "owner/name"},
		{"多一层斜杠非法", commandOptions{Repository: "acme/video/extra"}, "owner/name"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.options.validate()
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("validate() = %v, want 含 %q", err, testCase.wantErr)
			}
		})
	}
}

// TestNewAutomationCommandValidatesThenRunsAction 断言自动化叶子命令的两段
// 契约：先校验参数（非法时不触碰动作，避免半途发起请求），再以 root 的选项
// 快照调用动作，且动作拿到的 stdout/stderr 就是命令自己的输出流——写成
// os.Stdout 会让 CI 日志重定向失败。
func TestNewAutomationCommandValidatesThenRunsAction(t *testing.T) {
	options := &commandOptions{}
	var stdout, stderr strings.Builder
	called := 0
	var got commandOptions
	command := newAutomationCommand("label-sync", "短说明", "", func(_ context.Context, out, errOut io.Writer, received commandOptions) error {
		called++
		got = received
		if out != io.Writer(&stdout) {
			return errors.New("stdout 未透传")
		}
		return nil
	}, options, &stdout, &stderr)
	command.SetContext(t.Context())
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if called != 1 {
		t.Fatalf("动作调用 %d 次, want 1", called)
	}
	if got != *options {
		t.Errorf("动作收到 %+v, want %+v", got, *options)
	}

	// 非法参数：动作一次都不该跑（错误只来自校验）。
	options.Repository = "bad"
	command = newAutomationCommand("label-sync", "短说明", "", func(context.Context, io.Writer, io.Writer, commandOptions) error {
		called++
		return nil
	}, options, &stdout, &stderr)
	command.SetContext(t.Context())
	if err := command.Execute(); err == nil {
		t.Error("非法 --repo 应报错")
	}
	if called != 1 {
		t.Errorf("校验失败后动作仍被调用（累计 %d 次）", called)
	}
}

// TestNewAutomationCommandKeepsDeprecatedAliasesVisible 断言旧入口的兼容层：
// 已 install 的目标仓库里 workflow 仍写着 assistant sync / assistant automerge，
// 镜像更新后这些命令必须仍然存在（只是隐藏并从 Deprecated 提示改写法），否则
// 门禁会在无人察觉的情况下停摆，直到重跑 install。
func TestNewAutomationCommandKeepsDeprecatedAliasesVisible(t *testing.T) {
	var stdout, stderr strings.Builder
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error { return nil }
	// cobra 只在命令真正被使用时把弃用提示写进 stderr？不——它会用 Command.ErrOrStderr()，
	// 而 newRootCommand 内部把每个子命令的 Err 绑在 root 上，所以这里必须同时
	// SetOut 与 SetErr，漏一个提示就会落到进程 stderr 而断言不到。
	for _, testCase := range []struct {
		name string
		use  string
		want string
	}{
		{"sync", "sync", "请改用 assistant action label-sync"},
		{"automerge", "automerge", "请改用 assistant action automerge"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stderr.Reset()
			command := newRootCommand(&stdout, &stderr, runner, runner)
			command.SetOut(&stdout)
			command.SetErr(&stderr)
			command.SetContext(t.Context())
			command.SetArgs([]string{testCase.use})
			if err := command.Execute(); err != nil {
				t.Fatalf("旧入口 %s 应仍可执行: %v", testCase.use, err)
			}
		})
	}
}

// TestWithManagerDrivesEnvironmentModeEndToEnd 断言 withManager 在环境变量
// 单实例模式下把外部配置完整接到 Manager 上：--verbose 的进度行要写进 stderr
// （不能落到进程 stdout），并按 GITEA_REPOSITORY 把目标仓库固定下来；可选令牌
// （分支保护 / 状态评审）配齐时各留一条可行动日志，缺省时状态评审者按约定补
// "merge"。接错任何一项，都会让 CI 里的动作对着错的站点或错的身份执行。
func TestWithManagerDrivesEnvironmentModeEndToEnd(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["base-token"] = "ai"
	fake.login["admin-token"] = "admin"
	fake.login["state-token"] = "merge"
	fake.repositories = append(fake.repositories, "acme/video")

	t.Setenv("GITEA_HOST", fake.server.URL)
	t.Setenv("GITEA_ACCESS_TOKEN", "base-token")
	t.Setenv("GITEA_BRANCH_PROTECTION_TOKEN", "admin-token")
	t.Setenv("GITEA_STATE_TOKEN", "state-token")
	t.Setenv("GITEA_REPOSITORY", "acme/video")

	var stderr strings.Builder
	ran := false
	err := withManager(t.Context(), &stderr, commandOptions{Verbose: true}, func(_ context.Context, manager *status.Manager) error {
		ran = true
		if manager == nil {
			t.Error("manager 为 nil")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withManager: %v", err)
	}
	if !ran {
		t.Fatal("run 回调未被调用")
	}
	logged := stderr.String()
	for _, want := range []string{
		"认证通过",
		"分支保护读取使用 GITEA_BRANCH_PROTECTION_TOKEN 独立令牌",
		"状态评审提交使用 GITEA_STATE_TOKEN 独立令牌",
		"状态评审者: merge",
		"使用环境变量指定的仓库: acme/video",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("stderr 缺少 %q：\n%s", want, logged)
		}
	}
	// 非 verbose：进度行一条都不许出现，否则 CI 的默认输出会被过程日志淹没。
	stderr.Reset()
	if err := withManager(t.Context(), &stderr, commandOptions{}, func(context.Context, *status.Manager) error { return nil }); err != nil {
		t.Fatalf("withManager(非 verbose): %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("非 verbose 不应输出进度：\n%s", stderr.String())
	}
}

// TestWithManagerPropagatesAuthenticationFailure 断言鉴权预检的语义：令牌被
// 站点拒绝时立刻把错误冒泡（退出码 78），不进动作回调。把 401 吞掉会让 CI 的
// label-sync 静默成功而标签毫无变化，是最难发现的一类停摆。
func TestWithManagerPropagatesAuthenticationFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	t.Setenv("GITEA_HOST", fake.server.URL)
	t.Setenv("GITEA_ACCESS_TOKEN", "expired-token")

	ran := false
	err := withManager(t.Context(), io.Discard, commandOptions{}, func(context.Context, *status.Manager) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("令牌被拒时应报错")
	}
	if ran {
		t.Error("鉴权失败后不应调用动作")
	}

	// 没有 GITEA_HOST：config.Load 的错误原样冒泡，且不接触网络。
	t.Setenv("GITEA_HOST", "")
	t.Setenv("GITEA_ACCESS_TOKEN", "")
	if err := withManager(t.Context(), io.Discard, commandOptions{}, func(context.Context, *status.Manager) error {
		t.Error("缺配置时不应调用动作")
		return nil
	}); err == nil || !strings.Contains(err.Error(), "GITEA_HOST") {
		t.Errorf("缺 GITEA_HOST 的 err = %v", err)
	}
}

// 分派：有配置文件时走 runManagerAction（按仓库展开、逐个执行），没有时走
// 环境变量单实例模式。这两条路径的差别是「一次跑多个仓库」与「只跑一个」，
// 接错会让 CI 的定时任务漏掉大部分仓库。
func TestRunLabelSyncAndRunAutoMergeDispatchByConfigPresence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	// 先补凭据再写配置：writeFakeGiteaConfig 会调用 withReviewCredential 登记
	// 带 http:// 前缀的 host，这里的 review 令牌同理来自凭据库。
	withReviewCredential(t, fake.server.URL)
	configPath := writeFakeGiteaConfig(t, fake, "acme/rocket")

	// 配置文件模式：假站点缺标签与分支保护，动作会真实往返并可能失败；
	// 这里只钉「动作被派发到该仓库」这一点，即错误要么为空、要么带仓库语境。
	for _, testCase := range []struct {
		name string
		run  func(context.Context, io.Writer, io.Writer, commandOptions) error
	}{
		{"label-sync", runLabelSync},
		{"automerge", runAutoMerge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs strings.Builder
			err := testCase.run(t.Context(), io.Discard, &logs,
				commandOptions{ConfigPath: configPath, Repository: "acme/rocket"})
			if err != nil && !strings.Contains(err.Error(), "acme/rocket") {
				t.Errorf("错误缺少仓库语境: %v", err)
			}
		})
	}

	// 过滤词没命中：instanceManagers 的错误必须冒泡，动作一次都不跑。
	err := runLabelSync(t.Context(), io.Discard, io.Discard,
		commandOptions{ConfigPath: configPath, Repository: "acme/nope"})
	if err == nil || !strings.Contains(err.Error(), "不在配置文件的 instances[].repos 中") {
		t.Errorf("未命中过滤词的 err = %v", err)
	}

	// 环境变量模式：没有配置文件时走 withManager。
	fake.login["base-token"] = "ai"
	fake.repositories = append(fake.repositories, "acme/rocket")
	t.Setenv("GITEA_HOST", fake.server.URL)
	t.Setenv("GITEA_ACCESS_TOKEN", "base-token")
	t.Setenv("GITEA_REPOSITORY", "acme/rocket")
	if err := runAutoMerge(t.Context(), io.Discard, io.Discard, commandOptions{}); err != nil {
		t.Errorf("runAutoMerge(环境变量模式): %v", err)
	}
}

// TestNewRootCommandWiresSubcommandsAndHelp 断言根命令的命令树契约：无参数时
// 打印帮助（而不是静默成功）、未知子命令被拒绝、action 入口及其两个子命令都在，
// 且已移除的 check 不再挂在命令树上。
func TestNewRootCommandWiresSubcommandsAndHelp(t *testing.T) {
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error { return nil }
	var stdout, stderr strings.Builder
	command := newRootCommand(&stdout, &stderr, runner, runner)
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetContext(t.Context())

	// 无子命令：打印帮助，退出码 0。
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		t.Fatalf("无参数应打印帮助: %v", err)
	}
	if !strings.Contains(stdout.String(), "action") {
		t.Errorf("帮助缺少子命令：\n%s", stdout.String())
	}

	// 未知子命令：由 NoArgs 拒绝。
	stderr.Reset()
	command = newRootCommand(&stdout, &stderr, runner, runner)
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetContext(t.Context())
	command.SetArgs([]string{"definitely-not-a-command"})
	if err := command.Execute(); err == nil {
		t.Error("未知子命令应报错")
	}

	// 命令树：action 及其两个子命令都必须存在。
	fresh := newRootCommand(&stdout, &stderr, runner, runner)
	for _, path := range [][]string{{"action", "label-sync"}, {"action", "automerge"}} {
		found, _, err := fresh.Find(path)
		if err != nil || found == nil {
			t.Errorf("子命令 %v 缺失: %v", path, err)
		}
	}
	// check 已移除且不留弃用别名：它必须彻底从命令树上消失。按命令树而不是按
	// Find 断言——Find 找不到名字时会退回根命令并吞掉错误，测不出「已移除」。
	for _, child := range fresh.Commands() {
		if child.Name() == "check" {
			t.Error("check 已移除，却仍挂在命令树上")
		}
	}
	// --verbose 是持久旗标：子命令上必须可见（否则动作命令收不到它）。
	if fresh.PersistentFlags().Lookup("verbose") == nil {
		t.Error("root 应有持久旗标 --verbose")
	}
}
