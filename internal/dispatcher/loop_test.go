package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// planDeps 构造 dry-run 用依赖：副作用函数一律「即失败」桩。
func planDeps(t *testing.T, api API, config Config, logs *[]string) Deps {
	t.Helper()
	deps := Deps{
		Config:      config,
		API:         api,
		RepoDir:     "/repo",
		Log:         func(line string) { *logs = append(*logs, line) },
		BuildPrompt: BuildPrompt,
		PrepareWorktree: func(int64) (string, error) {
			t.Fatal("dry-run 不得创建 worktree")
			return "", nil
		},
		RemoveWorktree: func(string) error {
			t.Fatal("dry-run 不得移除 worktree")
			return nil
		},
		RunSession: func(SessionRequest) SessionOutcome {
			t.Fatal("dry-run 不得启动会话")
			return SessionOutcome{}
		},
	}
	if config.SyncMirror {
		deps.SyncMirror = func() (string, error) { return "abc1234", nil }
	}
	return deps
}

func TestDryRunPassListsPlanWithoutSideEffects(t *testing.T) {
	var logs []string
	api := &fakeAPI{
		requestedPulls: []status.PullRequest{{Index: 67, Title: "fix: draft 状态"}},
		issues:         []status.Issue{{Index: 66, Title: "ci: 分层重构"}},
	}
	deps := planDeps(t, api, testConfig(), &logs)
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	output := strings.Join(logs, "\n")
	worktreeDir := filepath.Join("worktrees", "pr-67")
	for _, want := range []string{
		"pull#67",
		"issue#66",
		"git fetch --quiet origin refs/pull/67/head",
		"git worktree add --quiet --detach " + worktreeDir,
		"cp -r /repo/.claude " + worktreeDir + "/.claude",
		"cd " + worktreeDir,
		"claude -p 'review pr #67",
		"--strict-mcp-config",
		"curl -s -H",
		"/pulls/67/reviews?limit=50",
		"head 漂移即本轮作废",
		"同一请求只拉起一个会话",
		"git worktree remove --force " + worktreeDir,
		"cwd=宿主检出根",
		"triage issue #66",
		"/issues/66/labels",
		"60000ms（1 分钟",
		"未创建 worktree、未启动会话、未提交 review",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}
}

func TestDryRunPassWithoutWork(t *testing.T) {
	var logs []string
	deps := planDeps(t, &fakeAPI{}, testConfig(), &logs)
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	output := strings.Join(logs, "\n")
	for _, want := range []string{"配置预览：host=", "令牌=***", "当前无待办"} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}
	if got := logs[len(logs)-1]; got != "当前无待办" {
		t.Errorf("最后一行 = %q, want 当前无待办", got)
	}
}

func TestDryRunPassWithSyncMirror(t *testing.T) {
	var logs []string
	config := testConfig(func(config *Config) { config.SyncMirror = true })
	deps := planDeps(t, &fakeAPI{}, config, &logs)
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	output := strings.Join(logs, "\n")
	for _, want := range []string{"每轮先同步镜像（--sync-mirror）", "checkout -f -B main origin/main", "clean -fd"} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}

	var plain []string
	if err := DryRunPass(context.Background(), planDeps(t, &fakeAPI{}, testConfig(), &plain)); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	if strings.Contains(strings.Join(plain, "\n"), "同步镜像") {
		t.Errorf("plain dry-run should not mention mirror: %v", plain)
	}
}

func TestProcessItemRoutesProgressToConsoleAndLog(t *testing.T) {
	logDir := t.TempDir()
	var logs []string
	api := &fakeAPI{labels: []status.Label{}}
	deps := Deps{
		Config:      testConfig(func(config *Config) { config.LogDir = logDir }),
		API:         api,
		RepoDir:     "/repo",
		Log:         func(line string) { logs = append(logs, line) },
		BuildPrompt: BuildPrompt,
		PrepareWorktree: func(int64) (string, error) {
			t.Fatal("issue 待办不建 worktree")
			return "", nil
		},
		RemoveWorktree: func(string) error {
			t.Fatal("issue 待办不清理 worktree")
			return nil
		},
		RunSession: func(request SessionRequest) SessionOutcome {
			onProgress := request.OnProgress
			onProgress("session=s-1 model=test")
			onProgress("开始审查")
			onProgress("🔧 Read")
			onProgress("🔧 Bash")
			return SessionOutcome{
				Subtype:    "success",
				NumTurns:   4,
				CostUSD:    0.1,
				DurationMS: 100,
				SessionID:  "s-1",
				Result:     "done",
				Errors:     []string{},
			}
		},
	}
	result := ProcessItem(context.Background(), deps, WorkItem{Kind: KindIssue, Number: 66, Title: "ci: 分层重构"})
	if !result.Settled {
		t.Error("completed triage should settle the request")
	}

	consoleOutput := strings.Join(logs, "\n")
	for _, want := range []string{
		"issue#66 session=s-1 model=test",
		"issue#66 🔧 #1 Read",
		"issue#66 🔧 #2 Bash",
		"issue#66 完成",
	} {
		if !strings.Contains(consoleOutput, want) {
			t.Errorf("console output missing %q:\n%s", want, consoleOutput)
		}
	}
	if strings.Contains(consoleOutput, "开始审查") {
		t.Errorf("assistant text should not reach console:\n%s", consoleOutput)
	}
	detail := readLogFile(t, logDir, "issue-66-")
	for _, want := range []string{
		"[progress] session=s-1 model=test",
		"[progress] 开始审查",
		"[progress] 🔧 Read",
		"[progress] 🔧 Bash",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail log missing %q:\n%s", want, detail)
		}
	}
}

func TestProcessItemHeadDriftInvalidatesWithoutRetry(t *testing.T) {
	logDir := t.TempDir()
	var logs []string
	sessions := 0
	api := &fakeAPI{pull: status.PullRequest{HeadSHA: "sha-new"}}
	deps := Deps{
		Config:          testConfig(func(config *Config) { config.LogDir = logDir }),
		API:             api,
		RepoDir:         "/repo",
		Log:             func(line string) { logs = append(logs, line) },
		BuildPrompt:     BuildPrompt,
		PrepareWorktree: func(int64) (string, error) { return "sha-old", nil },
		RemoveWorktree:  func(string) error { return nil },
		RunSession: func(SessionRequest) SessionOutcome {
			sessions++
			return SessionOutcome{
				Subtype:    "success",
				NumTurns:   3,
				CostUSD:    0.05,
				DurationMS: 50,
				SessionID:  "s-9",
				Result:     "reviewed stale head",
				Errors:     []string{},
			}
		},
	}
	result := ProcessItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 67, Title: "fix: something"})
	if result.Settled {
		t.Error("head drift must not settle the request (new head needs a new session)")
	}

	if sessions != 1 {
		t.Errorf("sessions = %d, want 1（漂移后不再烧第 2 个会话）", sessions)
	}
	output := strings.Join(logs, "\n")
	for _, want := range []string{
		"head 已推进 sha-old → sha-new",
		"本轮放行，下一轮以新 head 重开",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("console output missing %q:\n%s", want, output)
		}
	}
}

func TestAcquireLockCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "run", "dispatcher.lock")
	if err := AcquireLock(lockFile, func(string) {}); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock = %q, want pid %d", got, os.Getpid())
	}
}

func TestAcquireLockTakesOverDeadPID(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dispatcher.lock")
	// PID 上限之外的数值在任何平台都不会存活
	if err := os.WriteFile(lockFile, []byte("2147483647\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs []string
	if err := AcquireLock(lockFile, func(line string) { logs = append(logs, line) }); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock = %q, want pid %d", got, os.Getpid())
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "接管残留锁") {
		t.Errorf("logs = %v, want 接管残留锁", logs)
	}
}

func TestAcquireLockRejectsLiveInstance(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dispatcher.lock")
	// PID 1 在 Linux 上恒存在且不属于当前进程：锁被存活实例持有，必须拒绝
	if err := os.WriteFile(lockFile, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := AcquireLock(lockFile, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "已在运行") {
		t.Errorf("AcquireLock() error = %v, want 已在运行", err)
	}
}

// AcquireLock 的两条本机失败出口：锁目录建不出来（路径被普通文件占住）与
// 锁文件建不出来（父目录只读）都必须原样上抛。二者一旦被吞掉，第二次启动
// 会以为自己拿到了锁、与在跑的实例同时消费同一批待办。
func TestAcquireLockSurfacesFilesystemFailures(t *testing.T) {
	t.Run("锁目录被文件占住", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "run")
		if err := os.WriteFile(blocker, []byte("占位\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := AcquireLock(filepath.Join(blocker, "dispatcher.lock"), func(string) {}); err == nil {
			t.Error("父路径是文件时应报错（MkdirAll 失败必须冒泡）")
		}
	})

	t.Run("锁文件不可创建", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 无视权限位，跳过")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if err := AcquireLock(filepath.Join(dir, "dispatcher.lock"), func(string) {}); err == nil {
			t.Error("只读目录下应报错（O_EXCL 打开失败必须冒泡）")
		}
	})
}

func TestReleaseLockRemovesFile(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dispatcher.lock")
	if err := AcquireLock(lockFile, func(string) {}); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	ReleaseLock(lockFile)
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed, err = %v", err)
	}
}

func readLogFile(t *testing.T, dir, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return string(content)
		}
	}
	t.Fatalf("no log file with prefix %q in %s", prefix, dir)
	return ""
}

// mentionPollInterval：抽检节奏跟随配置——比 5s 基频更激进的配置原样尊重，
// 更慢的配置取五分之一（下限 5s），保证抽检始终快于旧节奏；非法值回落基频。
func TestMentionPollInterval(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "零值回落基频", configured: 0, want: 5 * time.Second},
		{name: "负值回落基频", configured: -time.Minute, want: 5 * time.Second},
		{name: "等于基频", configured: 5 * time.Second, want: 5 * time.Second},
		{name: "比基频更激进：原样尊重", configured: time.Second, want: time.Second},
		{name: "略高于基频：取五分之一", configured: 20 * time.Second, want: 5 * time.Second},
		{name: "五分之一高于基频", configured: time.Minute, want: 12 * time.Second},
		{name: "五分之一低于基频：取下限", configured: 24 * time.Second, want: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentionPollInterval(tc.configured); got != tc.want {
				t.Errorf("mentionPollInterval(%v) = %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}

// humanDuration：只在整除时降级到更大的单位，否则停留毫秒——时长展示不能被
// 四舍五入成与配置不符的数字。
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		duration time.Duration
		want     string
	}{
		{2 * time.Hour, "2 小时"},
		{30 * time.Minute, "30 分钟"},
		{45 * time.Second, "45 秒"},
		{1500 * time.Millisecond, "1500 毫秒"},
		{0, "0 毫秒"},
		{90 * time.Minute, "90 分钟"},                  // 不是整小时 → 分钟
		{3_600_001 * time.Millisecond, "3600001 毫秒"}, // 差 1ms 不降级
	}
	for _, tc := range cases {
		if got := humanDuration(tc.duration); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.duration, got, tc.want)
		}
	}
}

// maskSecret 只露前后各 4 位；空值与短密钥全掩码，日志里不得出现可用凭据。
func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":             "(空)",
		"abc":          "***",
		"12345678":     "***",         // 恰好 8 位仍全掩码
		"123456789":    "1234***6789", // 9 位起露出前后 4 位
		"token-abcdef": "toke***cdef",
	}
	for secret, want := range cases {
		if got := maskSecret(secret); got != want {
			t.Errorf("maskSecret(%q) = %q, want %q", secret, got, want)
		}
	}
}

// stamp：日志文件名的时间戳格式固定为 yyyyMMdd-HHmmss（本地时区），
// 同一秒内的多次处理共用同一文件名前缀，操作者可按时间排序定位。
func TestStampFormat(t *testing.T) {
	got := stamp(time.Date(2026, 9, 17, 14, 36, 1, 0, time.UTC))
	if got != "20260917-143601" {
		t.Errorf("stamp() = %q, want 20260917-143601", got)
	}
}

// ctxDone 是优雅退出的判定：取消后为真，未取消为假（不得阻塞）。
func TestCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	if ctxDone(ctx) {
		t.Error("未取消时 ctxDone 应为假")
	}
	cancel()
	if !ctxDone(ctx) {
		t.Error("已取消时 ctxDone 应为真")
	}
}

// planSteps 的可选分支：每一项都是「当前配置真的生效」的证据（演练要能回答
// 会话用什么镜像、带哪些 provider/仓库 MCP、是否注入项目约定、模型是否覆盖）。
func TestPlanStepsOptionalBranches(t *testing.T) {
	t.Run("docker 会话与网络", func(t *testing.T) {
		var logs []string
		config := testConfig(func(config *Config) {
			config.DockerImage = "ghcr.io/x/review:latest"
			config.DockerNetwork = "host"
		})
		deps := planDeps(t, &fakeAPI{}, config, &logs)
		if err := DryRunPass(context.Background(), deps); err != nil {
			t.Fatalf("DryRunPass() error = %v", err)
		}
		output := strings.Join(logs, "\n")
		for _, want := range []string{"docker=ghcr.io/x/review:latest", "（network=host）"} {
			if !strings.Contains(output, want) {
				t.Errorf("输出缺 %q:\n%s", want, output)
			}
		}
	})

	t.Run("env 单实例模式无 config 来源", func(t *testing.T) {
		var logs []string
		config := testConfig(func(config *Config) { config.ConfigPath = "" })
		deps := planDeps(t, &fakeAPI{}, config, &logs)
		if err := DryRunPass(context.Background(), deps); err != nil {
			t.Fatalf("DryRunPass() error = %v", err)
		}
		if output := strings.Join(logs, "\n"); !strings.Contains(output, "无（环境变量单实例模式）") {
			t.Errorf("输出缺 config 来源说明:\n%s", output)
		}
	})

	t.Run("provider 覆盖计入配置与 MCP 说明", func(t *testing.T) {
		var logs []string
		config := testConfig(func(config *Config) {
			config.ProviderName = "opencode"
			config.Provider = claudecfg.Overrides{
				Env:      map[string]string{"OPENAI_API_KEY": "k"},
				Settings: map[string]any{"model": "gpt-5"},
				MCP:      map[string]any{"provider-server": map[string]any{"command": "x"}},
			}
		})
		// planSteps 才带得到 --settings/--mcp-config 的说明文本，dry-run 空清单
		// 不会走到这里，所以直接对单个待办取步骤。
		steps := strings.Join(planSteps(planDeps(t, &fakeAPI{}, config, &logs), WorkItem{
			Kind: KindPull, Number: 1, Title: "补测",
		}), "\n")
		for _, want := range []string{
			// 配置说明要能回答「这份临时配置是谁给的」
			"+ provider opencode>",
			// MCP 说明要报出 provider 原生 server 的数量
			"provider opencode 的 1 个 server",
		} {
			if !strings.Contains(steps, want) {
				t.Errorf("步骤缺 %q:\n%s", want, steps)
			}
		}
	})

	t.Run("模型显式指定进入命令", func(t *testing.T) {
		var logs []string
		config := testConfig(func(config *Config) { config.Model = "opusplan" })
		steps := strings.Join(planSteps(planDeps(t, &fakeAPI{}, config, &logs), WorkItem{
			Kind: KindPull, Number: 1, Title: "补测",
		}), "\n")
		if !strings.Contains(steps, "--model opusplan") {
			t.Errorf("步骤缺 --model opusplan:\n%s", steps)
		}
	})

	t.Run("模型未指定回落账号默认", func(t *testing.T) {
		var logs []string
		deps := planDeps(t, &fakeAPI{}, testConfig(func(config *Config) { config.Model = "" }), &logs)
		if err := DryRunPass(context.Background(), deps); err != nil {
			t.Fatalf("DryRunPass() error = %v", err)
		}
		if output := strings.Join(logs, "\n"); !strings.Contains(output, "model=账号默认") {
			t.Errorf("输出缺账号默认:\n%s", output)
		}
	})
}

// planSteps 读取宿主检出的既有现场：仓库自带 .mcp.json（合并保留）、项目约定
// .assistant/review.md（追加系统提示）、以及同一待办的历史文本记录（改走
// --resume 续接）。三者都是仓库侧事实，必须反映在演练命令里。
func TestPlanStepsReadsRepoStateAndTranscript(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".assistant", "review.md"), []byte("# 约定"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := testConfig(func(config *Config) {
		config.SessionDir = t.TempDir()
		config.SessionProject = "-repo"
	})
	// 预置该待办的文本记录：dry-run 应据此给出 --resume 而不是 --session-id
	sessionID := SessionID(config.Host, config.Repository.FullName(), KindIssue, 66, "<head>")
	transcript := claudecfgTranscriptPathForTest(t, config.SessionDir, config.SessionProject, sessionID)

	var logs []string
	deps := planDeps(t, &fakeAPI{issues: []status.Issue{{Index: 66, Title: "ci"}}}, config, &logs)
	deps.RepoDir = repoDir
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	output := strings.Join(logs, "\n")
	for _, want := range []string{
		"仓库 .mcp.json 的其它 server",
		"--append-system-prompt <项目约定 .assistant/review.md>",
		"--resume " + sessionID,
		"文本记录：" + transcript,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("输出缺 %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "--session-id "+sessionID) {
		t.Error("已有文本记录时不得再下行 --session-id（会另起新会话，丢掉上下文）")
	}
}

// claudecfgTranscriptPathForTest 建出文本记录文件并返回其路径，用于演练的
// --resume 分支（生产侧路径计算与会话内一致）。
func claudecfgTranscriptPathForTest(t *testing.T, sessionDir, project, sessionID string) string {
	t.Helper()
	path := claudecfg.TranscriptPath(sessionDir, project, sessionID)
	if path == "" {
		t.Fatal("TranscriptPath 返回空，无法验证 --resume 分支")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// followUp 是全量会话后的兜底续读：把会话期间/验证之后新到的他人评论喂给同一
// 会话（--resume 续聊），直到没有未读或达到轮次上限。上限存在的意义是防死
// 循环：超限放行，下一轮检测重新处理（重新检测不会丢消息——水位线未推进）。
func TestFollowUpRoundsAndGuards(t *testing.T) {
	item := WorkItem{Kind: KindIssue, Number: 6, Title: "冒烟"}

	t.Run("每轮读走一条直到清空", func(t *testing.T) {
		remaining := []string{"@Ge：一", "@Ge：二"}
		sessions := 0
		var logs []string
		var progressLines []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				if len(remaining) == 0 {
					return nil, nil
				}
				message := remaining[0]
				remaining = remaining[1:]
				return []string{message}, nil
			},
			PostFollowUpNote: func(context.Context, WorkItem, int) error {
				// 说明评论走 Deps.PostFollowUpNote，不经 RunSession 的 request 出口
				return nil
			},
			RunSession: func(request SessionRequest) SessionOutcome {
				sessions++
				request.OnProgress("会话进度")
				return SessionOutcome{Subtype: "success", NumTurns: 1}
			},
		}
		deps.followUp(context.Background(), item, item.label(), time.Now(), "/repo",
			func(line string) { progressLines = append(progressLines, line) })

		if sessions != 2 {
			t.Fatalf("sessions = %d, want 2（两条消息各一轮，之后空即停）", sessions)
		}
		joined := strings.Join(logs, "\n")
		if !strings.Contains(joined, "有 1 条后续消息，追问轮 1/3") ||
			!strings.Contains(joined, "有 1 条后续消息，追问轮 2/3") {
			t.Errorf("logs 缺轮次标注:\n%s", joined)
		}
		detail := strings.Join(progressLines, "\n")
		for _, want := range []string{"[follow-up] @Ge：一", "[follow-up prompt]", "[follow-up progress] 会话进度", "[follow-up result] subtype=success"} {
			if !strings.Contains(detail, want) {
				t.Errorf("待办日志缺 %q:\n%s", want, detail)
			}
		}
	})

	t.Run("消息源源不断时封顶到上限", func(t *testing.T) {
		sessions := 0
		var logs []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				return []string{"@Ge：永不停"}, nil
			},
			RunSession: func(SessionRequest) SessionOutcome {
				sessions++
				return SessionOutcome{Subtype: "success"}
			},
		}
		deps.followUp(context.Background(), item, item.label(), time.Now(), "/repo", func(string) {})
		if sessions != maxFollowUpRounds {
			t.Fatalf("sessions = %d, want %d（不得无限续读）", sessions, maxFollowUpRounds)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "追问轮 3/3") {
			t.Errorf("logs 缺末轮标注:\n%s", logs)
		}
	})

	t.Run("未接入检查：不动", func(t *testing.T) {
		deps := Deps{
			Log: func(string) { t.Error("未接入检查时不应有日志") },
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("未接入检查时不得起会话")
				return SessionOutcome{}
			},
		}
		deps.followUp(context.Background(), item, item.label(), time.Now(), "/repo", func(string) {})
	})

	t.Run("读取失败：报告后放行", func(t *testing.T) {
		var logs []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				return nil, errors.New("gitea 504")
			},
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("读取失败不得起会话")
				return SessionOutcome{}
			},
		}
		deps.followUp(context.Background(), item, item.label(), time.Now(), "/repo", func(string) {})
		if len(logs) != 1 || !strings.Contains(logs[0], "后续消息检查失败") || !strings.Contains(logs[0], "gitea 504") {
			t.Errorf("logs = %v, want 检查失败 + 原错误", logs)
		}
	})

	t.Run("说明评论失败不影响续聊", func(t *testing.T) {
		remaining := []string{"@Ge：一"}
		sessions := 0
		var logs []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				if len(remaining) == 0 {
					return nil, nil
				}
				message := remaining[0]
				remaining = remaining[1:]
				return []string{message}, nil
			},
			PostFollowUpNote: func(context.Context, WorkItem, int) error {
				return errors.New("gitea 500")
			},
			RunSession: func(SessionRequest) SessionOutcome {
				sessions++
				return SessionOutcome{Subtype: "success"}
			},
		}
		deps.followUp(context.Background(), item, item.label(), time.Now(), "/repo", func(string) {})
		if sessions != 1 {
			t.Errorf("sessions = %d, want 1（评论失败不阻断续聊）", sessions)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "追问说明评论失败：gitea 500") {
			t.Errorf("logs 缺评论失败告警:\n%s", logs)
		}
	})

	t.Run("会话失败：报告后停止续读", func(t *testing.T) {
		rounds := 0
		var logs []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				rounds++
				return []string{"@Ge：追问"}, nil
			},
			RunSession: func(SessionRequest) SessionOutcome {
				return SessionOutcome{Subtype: "error_max_turns", IsError: true}
			},
		}
		deps.followUp(context.Background(), item, item.label(), time.Now(), "/repo", func(string) {})
		if rounds != 1 {
			t.Errorf("FollowUpMessages 调用 = %d, want 1（失败即停，不烧下一轮）", rounds)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "追问轮 1 失败（error_max_turns）") {
			t.Errorf("logs 缺失败标注:\n%s", logs)
		}
	})
}

// processFollowUp 是检测循环直派的追问轮（仅 mention 通道）：未接入检查、
// 读取失败、消息已消失、说明评论失败、互斥拒绝、会话失败各有独立收尾语义。
func TestProcessFollowUpBranches(t *testing.T) {
	item := WorkItem{Kind: KindIssue, Number: 6, Title: "冒烟", Mention: true, FollowUp: true, Since: time.Now()}

	t.Run("未接入新消息检查", func(t *testing.T) {
		var logs []string
		deps := Deps{Log: func(line string) { logs = append(logs, line) }}
		if result := deps.processFollowUp(context.Background(), item, "/repo", "", func(string) {}); result != (ProcessResult{}) {
			t.Errorf("result = %+v, want 空", result)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "未接入新消息检查") {
			t.Errorf("logs = %v, want 未接入说明", logs)
		}
	})

	t.Run("读取失败：不推进水位线", func(t *testing.T) {
		var logs []string
		var detail []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				return nil, errors.New("gitea 504")
			},
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("读取失败不得起会话")
				return SessionOutcome{}
			},
		}
		result := deps.processFollowUp(context.Background(), item, "/repo", "", func(line string) { detail = append(detail, line) })
		if result.Responded {
			t.Error("读取失败不得回报 Responded（否则水位线推进会吞掉消息）")
		}
		if !strings.Contains(strings.Join(logs, "\n"), "新消息读取失败：gitea 504") ||
			!strings.Contains(strings.Join(detail, "\n"), "[error] gitea 504") {
			t.Errorf("logs/detail 缺失败留痕: %v / %v", logs, detail)
		}
	})

	t.Run("消息已消失：仅推进水位线", func(t *testing.T) {
		var logs []string
		deps := Deps{
			Log: func(string) {},
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				return nil, nil
			},
			LogDebug: func(line string) { logs = append(logs, line) },
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("无消息不得起会话")
				return SessionOutcome{}
			},
		}
		result := deps.processFollowUp(context.Background(), item, "/repo", "", func(string) {})
		if !result.Responded || result.Settled {
			t.Errorf("result = %+v, want 仅 Responded", result)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "待回应消息已消失") {
			t.Errorf("logs = %v, want 消失说明", logs)
		}
	})

	t.Run("说明评论失败仍继续续聊", func(t *testing.T) {
		sessions := 0
		var logs []string
		deps := Deps{
			Log: func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) {
				return []string{"@Ge：追问"}, nil
			},
			PostFollowUpNote: func(context.Context, WorkItem, int) error {
				return errors.New("gitea 500")
			},
			RunSession: func(SessionRequest) SessionOutcome {
				sessions++
				return SessionOutcome{Subtype: "success", NumTurns: 1}
			},
		}
		result := deps.processFollowUp(context.Background(), item, "/repo", "sha-1", func(string) {})
		if !result.Responded || sessions < 1 {
			t.Errorf("result = %+v/sessions=%d, want 已回应", result, sessions)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "追问说明评论失败：gitea 500") {
			t.Errorf("logs 缺评论失败告警:\n%s", logs)
		}
	})

	t.Run("互斥拒绝：本轮放行", func(t *testing.T) {
		var logs []string
		deps := Deps{
			Log:              func(string) {},
			LogDebug:         func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) { return []string{"@Ge：追问"}, nil },
			OnStart:          func(WorkItem) bool { return false },
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("互斥拒绝后不得起会话")
				return SessionOutcome{}
			},
		}
		result := deps.processFollowUp(context.Background(), item, "/repo", "", func(string) {})
		if result.Responded {
			t.Error("互斥拒绝不得回报 Responded")
		}
		if !strings.Contains(strings.Join(logs, "\n"), "已有会话在处理") {
			t.Errorf("logs = %v, want 互斥说明", logs)
		}
	})

	t.Run("会话失败：不推进水位线", func(t *testing.T) {
		var logs []string
		var detail []string
		deps := Deps{
			Log:              func(line string) { logs = append(logs, line) },
			FollowUpMessages: func(context.Context, WorkItem, time.Time) ([]string, error) { return []string{"@Ge：追问"}, nil },
			RunSession: func(SessionRequest) SessionOutcome {
				return SessionOutcome{Subtype: "error_during_execution", IsError: true}
			},
		}
		result := deps.processFollowUp(context.Background(), item, "/repo", "", func(line string) { detail = append(detail, line) })
		if result.Responded {
			t.Error("会话失败不得回报 Responded")
		}
		if !strings.Contains(strings.Join(detail, "\n"), "[follow-up result] subtype=error_during_execution") {
			t.Errorf("detail 缺结果留痕:\n%s", strings.Join(detail, "\n"))
		}
		if !strings.Contains(strings.Join(logs, "\n"), "追问轮失败（error_during_execution）") {
			t.Errorf("logs 缺失败标注: %v", logs)
		}
	})
}

// runSessionRecorded 共用的「起会话」路径：互斥守卫、进度两路落点、结果上报
// 与待办日志的 attempt 记录。
func TestRunSessionRecordedBranches(t *testing.T) {
	item := WorkItem{Kind: KindPull, Number: 67, Title: "fix: x"}
	startedAt := time.Date(2026, 9, 17, 14, 36, 1, 0, time.UTC)

	t.Run("互斥拒绝不落日志", func(t *testing.T) {
		var debug []string
		var detail []string
		deps := Deps{
			Log:      func(string) {},
			LogDebug: func(line string) { debug = append(debug, line) },
			OnStart:  func(WorkItem) bool { return false },
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("互斥拒绝后不得起会话")
				return SessionOutcome{}
			},
		}
		_, started := deps.runSessionRecorded(item, SessionRequest{}, func(line string) { detail = append(detail, line) }, startedAt)
		if started {
			t.Error("started = true, want false")
		}
		if len(detail) != 0 {
			t.Errorf("detail = %v, want 空（未起会话不写 attempt）", detail)
		}
		if !strings.Contains(strings.Join(debug, "\n"), "已有会话在处理（互斥守卫）") {
			t.Errorf("debug = %v, want 互斥说明", debug)
		}
	})

	t.Run("进度分类落点与 denials 上报", func(t *testing.T) {
		var logs, verbose, debug, detail []string
		var finished SessionOutcome
		finishCalls := 0
		deps := Deps{
			Log:        func(line string) { logs = append(logs, line) },
			LogVerbose: func(line string) { verbose = append(verbose, line) },
			LogDebug:   func(line string) { debug = append(debug, line) },
			OnStart:    func(WorkItem) bool { return true },
			OnFinish: func(_ WorkItem, outcome SessionOutcome) {
				finishCalls++
				finished = outcome
			},
			RunSession: func(request SessionRequest) SessionOutcome {
				request.OnProgress("session=s-1 model=test")
				request.OnProgress("普通文本")
				request.OnProgress("🔧 Read")
				request.OnProgress("🔧 Bash")
				request.OnProgress("[debug] 完整命令行")
				request.OnProgress("[debug]事件 stream")
				return SessionOutcome{
					Subtype: "success", NumTurns: 3, CostUSD: 0.25,
					PermissionDenials: 2, SessionID: "s-1", TranscriptPath: "/t.jsonl",
					Result: "done", Errors: []string{}, Resumed: true,
				}
			},
		}
		outcome, started := deps.runSessionRecorded(item, SessionRequest{}, func(line string) { detail = append(detail, line) }, startedAt)
		if !started || outcome.SessionID != "s-1" {
			t.Fatalf("started/outcome = %t/%+v, want 已起会话", started, outcome)
		}
		if finishCalls != 1 || finished.SessionID != "s-1" {
			t.Errorf("OnFinish 调用/结果 = %d/%+v, want 1/s-1", finishCalls, finished)
		}

		joinedLogs := strings.Join(logs, "\n")
		if !strings.Contains(joinedLogs, "pull#67 session=s-1 model=test") {
			t.Errorf("console 缺 session 行:\n%s", joinedLogs)
		}
		if !strings.Contains(joinedLogs, "pull#67 会话结束：success turns=3 cost=$0.25 denials=2") {
			t.Errorf("console 缺结束行:\n%s", joinedLogs)
		}
		if strings.Contains(joinedLogs, "普通文本") {
			t.Errorf("assistant 文本不得上控制台:\n%s", joinedLogs)
		}
		if got := strings.Join(verbose, "\n"); !strings.Contains(got, "pull#67 🔧 #1 Read") || !strings.Contains(got, "pull#67 🔧 #2 Bash") {
			t.Errorf("verbose 缺工具计数:\n%s", got)
		}
		gotDebug := strings.Join(debug, "\n")
		if !strings.Contains(gotDebug, "pull#67 完整命令行") || !strings.Contains(gotDebug, "pull#67 [debug]事件 stream") {
			t.Errorf("debug 缺会话内部明细:\n%s", gotDebug)
		}

		joinedDetail := strings.Join(detail, "\n")
		for _, want := range []string{
			"[progress] session=s-1 model=test",
			"[progress] 🔧 Read",
			`"subtype":"success"`,
			`"permissionDenials":2`,
			`"resumed":true`,
		} {
			if !strings.Contains(joinedDetail, want) {
				t.Errorf("待办日志缺 %q:\n%s", want, joinedDetail)
			}
		}
	})
}

// verifyItem 是统一完成判定入口：PR 走 review 判定（带 head 漂移），Issue 走
// 标签判定；API 错误一律原样上抛（不得当成「未完成」，否则网络抖动会伪装成
// 「本轮无新 review」）。
func TestVerifyItemRoutesByKindAndPropagatesErrors(t *testing.T) {
	t.Run("PR 漂移", func(t *testing.T) {
		deps := Deps{
			Config: testConfig(),
			API:    &fakeAPI{pull: status.PullRequest{HeadSHA: "sha-2"}, reviews: freshReviews()},
		}
		verdict, err := verifyItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 58}, since, "sha-1")
		if err != nil {
			t.Fatalf("verifyItem() error = %v", err)
		}
		if verdict.completed || !verdict.headMoved {
			t.Errorf("verdict = %+v, want head moved", verdict)
		}
	})

	t.Run("PR review 查询失败原样上抛", func(t *testing.T) {
		deps := Deps{
			Config: testConfig(),
			API:    &fakeAPI{pullErr: errors.New("gitea 500")},
		}
		if _, err := verifyItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 58}, since, "sha-1"); err == nil ||
			!strings.Contains(err.Error(), "gitea 500") {
			t.Errorf("error = %v, want 原样上抛", err)
		}
	})

	t.Run("Issue 标签已收敛", func(t *testing.T) {
		deps := Deps{
			Config: testConfig(),
			API:    &fakeAPI{labels: []status.Label{{Name: "type/bug"}}},
		}
		verdict, err := verifyItem(context.Background(), deps, WorkItem{Kind: KindIssue, Number: 62}, since, "")
		if err != nil {
			t.Fatalf("verifyItem() error = %v", err)
		}
		if !verdict.completed {
			t.Errorf("verdict = %+v, want completed", verdict)
		}
	})

	t.Run("Issue 标签查询失败原样上抛", func(t *testing.T) {
		deps := Deps{
			Config: testConfig(),
			API:    &fakeAPI{labelsErr: errors.New("gitea 502")},
		}
		if _, err := verifyItem(context.Background(), deps, WorkItem{Kind: KindIssue, Number: 62}, since, ""); err == nil ||
			!strings.Contains(err.Error(), "gitea 502") {
			t.Errorf("error = %v, want 原样上抛", err)
		}
	})
}

// ProcessItem 的失败与短路路径：镜像同步失败跳过本条；worktree 准备失败即
// 收敛；完成判定失败仍如实回报 Responded（会话本身没出错，水位线该推进）。
func TestProcessItemFailurePaths(t *testing.T) {
	t.Run("镜像同步失败：跳过本条，不建 worktree", func(t *testing.T) {
		var logs []string
		deps := Deps{
			Config:  testConfig(func(config *Config) { config.LogDir = t.TempDir() }),
			API:     &fakeAPI{},
			RepoDir: "/repo",
			Log:     func(line string) { logs = append(logs, line) },
			SyncMirror: func() (string, error) {
				return "", errors.New("fetch 失败")
			},
			PrepareWorktree: func(int64) (string, error) {
				t.Error("同步失败不得建 worktree")
				return "", nil
			},
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("同步失败不得起会话")
				return SessionOutcome{}
			},
			RemoveWorktree: func(string) error { return nil },
			BuildPrompt:    BuildPrompt,
		}
		result := ProcessItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 67, Title: "fix"})
		if result != (ProcessResult{}) {
			t.Errorf("result = %+v, want 空（跳过本条）", result)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "镜像同步失败：fetch 失败（跳过本条，下一轮重试）") {
			t.Errorf("logs = %v, want 同步失败说明", logs)
		}
	})

	t.Run("worktree 准备失败即收敛", func(t *testing.T) {
		var logs []string
		removed := []string{}
		deps := Deps{
			Config:          testConfig(func(config *Config) { config.LogDir = t.TempDir() }),
			API:             &fakeAPI{},
			RepoDir:         "/repo",
			Log:             func(line string) { logs = append(logs, line) },
			PrepareWorktree: func(int64) (string, error) { return "", errors.New("refs/pull/67/head 不存在") },
			RemoveWorktree:  func(dir string) error { removed = append(removed, dir); return nil },
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("准备失败不得起会话")
				return SessionOutcome{}
			},
			BuildPrompt: BuildPrompt,
		}
		result := ProcessItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 67, Title: "fix"})
		if result != (ProcessResult{}) {
			t.Errorf("result = %+v, want 空", result)
		}
		// defer 里清的是按命名约定推出的 worktreeDir（即便准备失败也会清一次，
		// 幂等：目标是派生的固定路径，不是仓库根）
		if len(removed) != 1 || removed[0] != "worktrees/pr-67" {
			t.Errorf("removed = %v, want 仅派生路径 worktrees/pr-67", removed)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "处理异常：refs/pull/67/head 不存在") {
			t.Errorf("logs = %v, want 处理异常", logs)
		}
	})

	t.Run("完成判定失败：仍回报 Responded", func(t *testing.T) {
		var logs []string
		deps := Deps{
			Config:          testConfig(func(config *Config) { config.LogDir = t.TempDir() }),
			API:             &fakeAPI{pullErr: errors.New("gitea 500")},
			RepoDir:         "/repo",
			Log:             func(line string) { logs = append(logs, line) },
			PrepareWorktree: func(int64) (string, error) { return "sha-1", nil },
			RemoveWorktree:  func(string) error { return nil },
			BuildPrompt:     BuildPrompt,
			RunSession: func(SessionRequest) SessionOutcome {
				return SessionOutcome{Subtype: "success", NumTurns: 1}
			},
		}
		result := ProcessItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 67, Title: "fix"})
		if !result.Responded || result.Settled {
			t.Errorf("result = %+v, want Responded 且未 Settled", result)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "处理异常：gitea 500") {
			t.Errorf("logs = %v, want 处理异常", logs)
		}
	})

	t.Run("会话已在处理：本轮放行且清掉 worktree", func(t *testing.T) {
		var logs, verbose []string
		removed := []string{}
		deps := Deps{
			Config:          testConfig(func(config *Config) { config.LogDir = t.TempDir() }),
			API:             &fakeAPI{},
			RepoDir:         "/repo",
			Log:             func(line string) { logs = append(logs, line) },
			LogVerbose:      func(line string) { verbose = append(verbose, line) },
			SyncMirror:      func() (string, error) { return "abc1234", nil },
			PrepareWorktree: func(int64) (string, error) { return "sha-1", nil },
			RemoveWorktree:  func(dir string) error { removed = append(removed, dir); return nil },
			BuildPrompt:     BuildPrompt,
			// 互斥守卫拒绝：同一待办已有在跑的会话，本轮不得再起
			OnStart: func(WorkItem) bool { return false },
			RunSession: func(SessionRequest) SessionOutcome {
				t.Error("互斥拒绝后不得起会话")
				return SessionOutcome{}
			},
		}
		result := ProcessItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 67, Title: "fix"})
		if result != (ProcessResult{}) {
			t.Errorf("result = %+v, want 空（本轮放行）", result)
		}
		// worktree 已建出：defer 必须清掉，否则下一轮同名 add 失败
		if len(removed) != 1 || removed[0] != "worktrees/pr-67" {
			t.Errorf("removed = %v, want 清掉派生的 worktree", removed)
		}
		// 同步成功走 verbose（不是 default）：正常细节不给操作者刷屏
		if !strings.Contains(strings.Join(verbose, "\n"), "pull#67 镜像同步：main @ abc1234") {
			t.Errorf("verbose 缺同步成功行:\n%s", strings.Join(verbose, "\n"))
		}
		if strings.Contains(strings.Join(logs, "\n"), "镜像同步") {
			t.Errorf("同步成功不得进 default 输出:\n%s", strings.Join(logs, "\n"))
		}
	})
}
