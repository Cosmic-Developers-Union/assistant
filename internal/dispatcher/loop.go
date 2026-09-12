// 主循环：检测待办 → 逐项（prepare → 会话 → 验证 → 清理）→ 空闲轮询。
//
//   - 单飞：同一时刻至多一个评审会话；lockfile 记 PID，宿主 PID 存活时拒绝
//     启动第二实例，死 PID（上次崩溃残留）自动接管。
//   - 优雅退出：首个 SIGINT/SIGTERM 置 stopping——sleep 立即返回、当前待办
//     处理完即退出；二次信号视为强杀（进程退出，残留锁下次启动接管）。
//   - 验证失败重试一次（每待办至多两个会话），仍失败则记录并放行，等待下一轮
//     检测（标签未变意味着待办仍在列表里，循环天然重试）。
package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Deps 是主循环的依赖：CLI 注入真实实现，测试注入桩。
type Deps struct {
	Config      Config
	API         API
	RepoDir     string
	Log         func(string)
	BuildPrompt func(kind string, number int64, extra PromptContext) string
	// CurrentLogin 查询令牌所属账号（启动横幅展示 + reviewer 不一致警告）；
	// 未传则横幅省略账户行
	CurrentLogin func(context.Context) (string, error)
	// SyncMirror 每轮检测前把宿主检出对齐 origin 基线；未开启时为 nil
	SyncMirror      func() (string, error)
	PrepareWorktree func(pullNumber int64) (string, error)
	RemoveWorktree  func(dir string) error
	RunSession      func(prompt, cwd string, onProgress func(string)) SessionOutcome
}

// AcquireLock 单飞锁：已存活实例在跑则报错，死 PID 残留则接管。锁文件写入用
// O_CREATE|O_EXCL 保证原子性。
func AcquireLock(lockFile string, log func(string)) error {
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		return err
	}
	for {
		file, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, writeErr := fmt.Fprintf(file, "%d\n", os.Getpid())
			closeErr := file.Close()
			if writeErr != nil {
				return writeErr
			}
			return closeErr
		}
		if !os.IsExist(err) {
			return err
		}
		raw, readErr := os.ReadFile(lockFile)
		display := "无效"
		existing := 0
		if readErr == nil {
			if value, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
				existing = value
				display = strconv.Itoa(value)
			}
		}
		if existing > 0 && existing != os.Getpid() && pidAlive(existing) {
			return fmt.Errorf("dispatcher 已在运行（PID %d，锁 %s）", existing, lockFile)
		}
		log(fmt.Sprintf("接管残留锁（记录 PID %s 已不存在）", display))
		if err := os.Remove(lockFile); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
}

// ReleaseLock 释放单飞锁（一次性命令 review/triage 用；RunLoop 退出时自管）。
func ReleaseLock(lockFile string) {
	_ = os.Remove(lockFile)
}

// pidAlive 探测进程存活（signal 0 不实际发送）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func stamp(now time.Time) string {
	return now.Format("20060102-150405")
}

func signalLabel(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return sig.String()
}

// RunLoop 是常驻主循环（部署形态）。
func RunLoop(ctx context.Context, deps Deps) error {
	config := deps.Config
	var stopping atomic.Bool
	stopCh := make(chan struct{})
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		count := 0
		for sig := range signals {
			count++
			if count == 1 {
				stopping.Store(true)
				deps.Log(fmt.Sprintf("收到 %s，处理完当前待办后退出（再次发送将立即强杀）", signalLabel(sig)))
				close(stopCh)
				continue
			}
			deps.Log(fmt.Sprintf("再次收到 %s，强杀退出", signalLabel(sig)))
			os.Exit(1)
		}
	}()

	if err := AcquireLock(config.LockFile, deps.Log); err != nil {
		return err
	}
	if err := os.MkdirAll(config.LogDir, 0o755); err != nil {
		ReleaseLock(config.LockFile)
		return err
	}
	if err := os.MkdirAll(config.WorktreeRoot, 0o755); err != nil {
		ReleaseLock(config.LockFile)
		return err
	}
	defer ReleaseLock(config.LockFile)

	sleep := func(duration time.Duration) {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-stopCh:
		}
	}

	// 启动横幅：一眼确认「谁在跑、写哪里、按什么规则」
	if deps.CurrentLogin != nil {
		account, err := deps.CurrentLogin(ctx)
		if err != nil {
			deps.Log(fmt.Sprintf("账户查询失败：%v", err))
		} else {
			deps.Log(
				fmt.Sprintf("dispatcher 启动：host=%s repo=%s 账户=@%s reviewer=%s 并发=%d interval=%dms",
					config.Host, config.Repository.FullName(), account, config.Reviewer, config.Concurrency, config.Interval.Milliseconds()),
			)
			if account != config.Reviewer {
				deps.Log(fmt.Sprintf(
					"警告：令牌账户 @%s ≠ reviewer %s——完成判定按 reviewer 提交的新 review 匹配，该配置下待办永远不会被判定完成",
					account, config.Reviewer,
				))
			}
		}
	}
	if deps.CurrentLogin == nil {
		deps.Log(
			fmt.Sprintf("dispatcher 启动：host=%s repo=%s reviewer=%s 并发=%d interval=%dms",
				config.Host, config.Repository.FullName(), config.Reviewer, config.Concurrency, config.Interval.Milliseconds()),
		)
	}
	model := ""
	if config.Model != "" {
		model = " model=" + config.Model
	}
	deps.Log(fmt.Sprintf(
		"  会话：claude=%s%s timeout=%dms（%s）",
		config.ClaudeBin, model, config.SessionTimeout.Milliseconds(), humanDuration(config.SessionTimeout),
	))
	deps.Log(fmt.Sprintf("  落点：logs=%s worktrees=%s lock=%s", config.LogDir, config.WorktreeRoot, config.LockFile))
	mirror := "关"
	if config.SyncMirror {
		mirror = "开"
	}
	deps.Log(fmt.Sprintf(
		"  基线：origin/%s（镜像同步%s） 漂移规则：head 推进即本轮作废",
		config.BaseBranch, mirror,
	))

	for !stopping.Load() {
		// 镜像同步先行（fail-closed）：检出是评审标准与分诊的数据源，与其带着
		// 陈旧基线评审，不如跳过本轮等待重试
		if deps.SyncMirror != nil {
			sha, err := deps.SyncMirror()
			if err != nil {
				deps.Log(fmt.Sprintf(
					"镜像同步失败：%v（跳过本轮，%dms 后重试）",
					err, config.Interval.Milliseconds(),
				))
				sleep(config.Interval)
				continue
			}
			deps.Log(fmt.Sprintf("镜像同步：%s @ %s", config.BaseBranch, sha))
		}
		work, err := ListWork(ctx, deps.API, config.Repository)
		if err != nil {
			deps.Log(fmt.Sprintf(
				"检测失败：%v（%dms 后重试）",
				err, config.Interval.Milliseconds(),
			))
			sleep(config.Interval)
			continue
		}
		if len(work) == 0 {
			sleep(config.Interval)
			continue
		}
		labels := make([]string, 0, len(work))
		for _, item := range work {
			labels = append(labels, fmt.Sprintf("%s#%d", item.Kind, item.Number))
		}
		deps.Log(fmt.Sprintf("检测到 %d 个待办（并发 %d）：%s", len(work), config.Concurrency, strings.Join(labels, " ")))

		// 有界并发：轮内至多 concurrency 个会话，轮与轮之间天然是 barrier——
		// 同一待办同一时刻至多一个会话，同一待办的多次会话只发生在同一轮内
		var wg sync.WaitGroup
		var next int
		var mutex sync.Mutex
		worker := func() {
			defer wg.Done()
			for !stopping.Load() {
				mutex.Lock()
				if next >= len(work) {
					mutex.Unlock()
					return
				}
				index := next
				next++
				mutex.Unlock()
				ProcessItem(ctx, deps, work[index])
			}
		}
		workers := config.Concurrency
		if workers > len(work) {
			workers = len(work)
		}
		for range workers {
			wg.Add(1)
			go worker()
		}
		wg.Wait()
		// 一轮处理后歇一个间隔再重新检测：等 gitea-assistant 的标签收敛，
		// 也避免卡死待办（验证不通过但标签未变）被连续重开会话
		sleep(config.Interval)
	}
	deps.Log("已退出主循环")
	return nil
}

// humanDuration 把时长折成人类可读形式（1800000ms → '30 分钟'），演练输出用。
func humanDuration(duration time.Duration) string {
	ms := duration.Milliseconds()
	switch {
	case ms >= 3_600_000 && ms%3_600_000 == 0:
		return fmt.Sprintf("%d 小时", ms/3_600_000)
	case ms >= 60_000 && ms%60_000 == 0:
		return fmt.Sprintf("%d 分钟", ms/60_000)
	case ms >= 1_000 && ms%1_000 == 0:
		return fmt.Sprintf("%d 秒", ms/1_000)
	}
	return fmt.Sprintf("%d 毫秒", ms)
}

// planSteps 按 ProcessItem 的真实执行序列给出具体命令清单（dry-run 只打印
// 不执行）：每行要么是原样可复制的命令，要么是 # 注释；<时间戳> /
// $(git rev-parse FETCH_HEAD) 等占位符表示运行时才确定的值。
func planSteps(deps Deps, item WorkItem) []string {
	config := deps.Config
	quote := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	curl := func(path string) string {
		return fmt.Sprintf("curl -s -H 'Authorization: token ***' %s/api/v1/repos/%s%s",
			config.Host, config.Repository.FullName(), path)
	}
	claudeArgs := []string{
		"--permission-mode auto",
		"--autocompact auto",
		"--output-format stream-json",
		"--verbose",
		"--strict-mcp-config",
		"--mcp-config " + filepath.Join(deps.RepoDir, ".mcp.json"),
		fmt.Sprintf("--max-turns %d", MaxTurns),
	}
	if config.Model != "" {
		claudeArgs = append(claudeArgs, "--model "+config.Model)
	}
	claude := func(cwdNote, promptFirstLine string) string {
		return fmt.Sprintf("claude -p %s %s   # %s；会话超时 %dms（%s，到点 SIGTERM、5s 后 SIGKILL）",
			quote(promptFirstLine+" …"), strings.Join(claudeArgs, " "), cwdNote,
			config.SessionTimeout.Milliseconds(), humanDuration(config.SessionTimeout))
	}

	if item.Kind == KindPull {
		worktreeDir := filepath.Join(config.WorktreeRoot, fmt.Sprintf("pr-%d", item.Number))
		return []string{
			fmt.Sprintf("# 待办日志：%s（提示词、会话进度、结果、验证结论）",
				filepath.Join(config.LogDir, fmt.Sprintf("pull-%d-<时间戳>.log", item.Number))),
			"cd " + deps.RepoDir,
			fmt.Sprintf("git fetch --quiet origin refs/pull/%d/head", item.Number),
			fmt.Sprintf("git worktree add --quiet --detach %s \"$(git rev-parse FETCH_HEAD)\"", worktreeDir),
			fmt.Sprintf("rm -rf %s/.claude && cp -r %s/.claude %s/.claude", worktreeDir, deps.RepoDir, worktreeDir) +
				"   # 评审标准钉定基线（PR 自带版本不生效）",
			"cd " + worktreeDir,
			claude("cwd=worktree", strings.Split(
				deps.BuildPrompt(KindPull, item.Number, PromptContext{Title: item.Title}), "\n")[0]),
			fmt.Sprintf("# 完成判定：head 漂移即本轮作废；reviewer %s 有新 review 才算完成", config.Reviewer),
			curl(fmt.Sprintf("/pulls/%d", item.Number)),
			curl(fmt.Sprintf("/pulls/%d/reviews?limit=50", item.Number)),
			"# 未完成且 head 未动：同 head 重试 1 次（本待办至多 2 个会话）；两轮耗尽放行，下一轮检测再处理",
			"cd " + deps.RepoDir,
			fmt.Sprintf("git worktree remove --force %s   # 无论会话成败", worktreeDir),
		}
	}
	return []string{
		fmt.Sprintf("# 待办日志：%s（提示词、会话进度、结果、验证结论）",
			filepath.Join(config.LogDir, fmt.Sprintf("issue-%d-<时间戳>.log", item.Number))),
		"cd " + deps.RepoDir,
		claude("cwd=宿主检出根", strings.Split(
			deps.BuildPrompt(KindIssue, item.Number, PromptContext{Title: item.Title}), "\n")[0]),
		fmt.Sprintf("# 完成判定：%s 标签已从该 Issue 移除", triageLabel),
		curl(fmt.Sprintf("/issues/%d/labels", item.Number)),
		"# 未完成：重试 1 次（本待办至多 2 个会话）；两轮耗尽放行，下一轮检测再处理",
	}
}

// DryRunPass 是 run 的只读演练：按同一检测口径列出将执行的待办，逐条给出
// 将执行的具体命令（只打印不执行）——不建 worktree、不起会话、不写待办日志。
func DryRunPass(ctx context.Context, deps Deps) error {
	config := deps.Config
	if deps.SyncMirror != nil {
		deps.Log(fmt.Sprintf(
			"每轮先同步镜像（--sync-mirror）：git fetch --prune origin；git checkout -f -B %s origin/%s；git clean -fd（在宿主检出，失败跳过本轮）",
			config.BaseBranch, config.BaseBranch,
		))
		deps.Log("")
	}
	work, err := ListWork(ctx, deps.API, config.Repository)
	if err != nil {
		return err
	}
	if len(work) == 0 {
		deps.Log("当前无待办")
		return nil
	}
	deps.Log(fmt.Sprintf("检测到 %d 个待办（dry-run 只读演练，将执行的具体命令如下，不会真正执行）：", len(work)))
	for _, item := range work {
		deps.Log("")
		deps.Log(fmt.Sprintf("%s#%d  %s", item.Kind, item.Number, item.Title))
		for _, step := range planSteps(deps, item) {
			deps.Log("  " + step)
		}
	}
	deps.Log("")
	deps.Log("dry-run 结束：未创建 worktree、未启动会话、未提交 review、未写待办日志。")
	return nil
}

type attemptRecord struct {
	Attempt           int64    `json:"attempt"`
	StartedAt         int64    `json:"startedAt"`
	Subtype           string   `json:"subtype"`
	IsError           bool     `json:"isError"`
	NumTurns          int      `json:"numTurns"`
	CostUSD           float64  `json:"costUsd"`
	DurationMS        int64    `json:"durationMs"`
	SessionID         string   `json:"sessionId"`
	Result            string   `json:"result"`
	Errors            []string `json:"errors"`
	PermissionDenials int      `json:"permissionDenials"`
}

// ProcessItem 处理单个待办（主循环逐项调用；review/triage 一次性命令也走这里）。
// 会话进度两路落点：控制台实时显示基础进度，完整明细实时写待办日志。
func ProcessItem(ctx context.Context, deps Deps, item WorkItem) {
	config := deps.Config
	tag := fmt.Sprintf("%s#%d", item.Kind, item.Number)
	logFile := filepath.Join(config.LogDir, fmt.Sprintf("%s-%d-%s.log", item.Kind, item.Number, stamp(time.Now())))
	appendLog := func(line string) {
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		_, _ = file.WriteString(line)
		_ = file.Close()
	}
	appendLog(fmt.Sprintf("[start] %s %s\n", tag, item.Title))

	cwd := deps.RepoDir
	var worktreeDir string
	var headSHA string
	defer func() {
		if worktreeDir != "" {
			_ = deps.RemoveWorktree(worktreeDir)
		}
	}()

	if item.Kind == KindPull {
		worktreeDir = filepath.Join(config.WorktreeRoot, fmt.Sprintf("pr-%d", item.Number))
		sha, err := deps.PrepareWorktree(item.Number)
		if err != nil {
			deps.Log(fmt.Sprintf("%s 处理异常：%v", tag, err))
			appendLog(fmt.Sprintf("[error] %v\n", err))
			return
		}
		headSHA = sha
		cwd = worktreeDir
		deps.Log(fmt.Sprintf("%s worktree=%s head=%s", tag, worktreeDir, headSHA))
	}
	prompt := deps.BuildPrompt(item.Kind, item.Number, PromptContext{Title: item.Title, HeadSHA: headSHA})
	appendLog("[prompt] " + strings.ReplaceAll(prompt, "\n", " ⏎ ") + "\n")

	for attempt := 1; attempt <= 2; attempt++ {
		// 工具调用编号按会话计数——跨会话累计会被误读成单会话长度
		toolCalls := 0
		onProgress := func(line string) {
			appendLog("[progress] " + line + "\n")
			switch {
			case strings.HasPrefix(line, "session="):
				deps.Log(tag + " " + line)
			case strings.HasPrefix(line, "🔧 "):
				toolCalls++
				deps.Log(fmt.Sprintf("%s 🔧 #%d %s", tag, toolCalls, strings.TrimPrefix(line, "🔧 ")))
			}
		}
		if attempt > 1 {
			deps.Log(fmt.Sprintf("%s 完成验证未通过，重试（第 %d 次，共 2 个会话）", tag, attempt))
		}
		startedAt := time.Now()
		outcome := deps.RunSession(prompt, cwd, onProgress)
		record, err := json.Marshal(attemptRecord{
			Attempt:           int64(attempt),
			StartedAt:         startedAt.UnixMilli(),
			Subtype:           outcome.Subtype,
			IsError:           outcome.IsError,
			NumTurns:          outcome.NumTurns,
			CostUSD:           outcome.CostUSD,
			DurationMS:        outcome.DurationMS,
			SessionID:         outcome.SessionID,
			Result:            outcome.Result,
			Errors:            outcome.Errors,
			PermissionDenials: outcome.PermissionDenials,
		})
		if err == nil {
			appendLog(string(record) + "\n")
		}
		denials := ""
		if outcome.PermissionDenials > 0 {
			denials = fmt.Sprintf(" denials=%d", outcome.PermissionDenials)
		}
		deps.Log(fmt.Sprintf("%s 会话结束：%s turns=%d cost=$%.2f%s",
			tag, outcome.Subtype, outcome.NumTurns, outcome.CostUSD, denials))

		verdict, err := verifyItem(ctx, deps, item, startedAt, headSHA)
		if err != nil {
			deps.Log(fmt.Sprintf("%s 处理异常：%v", tag, err))
			appendLog(fmt.Sprintf("[error] %v\n", err))
			return
		}
		appendLog(fmt.Sprintf("[verify] attempt=%d completed=%t headMoved=%t reason=%s\n",
			attempt, verdict.completed, verdict.headMoved, verdict.reason))
		if verdict.completed {
			deps.Log(fmt.Sprintf("%s 完成（%s）", tag, verdict.reason))
			return
		}
		// 作者在会话期间推送 ⇒ 评审锚定的旧 head 已作废：重试也只会再评旧代码，
		// 直接放行，下一轮以新 head 重开（会话锚定的 head 记录在待办日志）
		if verdict.headMoved {
			deps.Log(fmt.Sprintf("%s %s；本轮放行，下一轮以新 head 重开", tag, verdict.reason))
			return
		}
	}
	deps.Log(fmt.Sprintf("%s 两次会话后仍未检测到完成动作，放行等待下一轮", tag))
}

type itemVerdict struct {
	completed bool
	headMoved bool
	reason    string
}

// verifyItem 统一完成判定：PR 带 head 漂移检测（expectedHead 为会话开工时钉定的 head）。
func verifyItem(ctx context.Context, deps Deps, item WorkItem, since time.Time, expectedHead string) (itemVerdict, error) {
	if item.Kind == KindPull {
		verdict, err := VerifyPullReview(
			ctx, deps.API, deps.Config.Repository, deps.Config.Reviewer, item.Number, since, expectedHead,
		)
		if err != nil {
			return itemVerdict{}, err
		}
		return itemVerdict{completed: verdict.Completed, headMoved: verdict.HeadMoved, reason: verdict.Reason}, nil
	}
	verdict, err := VerifyIssueTriage(ctx, deps.API, deps.Config.Repository, item.Number)
	if err != nil {
		return itemVerdict{}, err
	}
	return itemVerdict{completed: verdict.Completed, reason: verdict.Reason}, nil
}
