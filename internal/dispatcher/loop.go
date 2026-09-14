// 主循环：检测待办 → 逐项（prepare → 会话 → 验证 → 清理）→ 空闲轮询。
//
//   - 单飞：同一时刻至多一个评审会话；lockfile 记 PID，宿主 PID 存活时拒绝
//     启动第二实例，死 PID（上次崩溃残留）自动接管。
//   - 优雅退出：ctx 取消（首个 SIGINT/SIGTERM，由 main 统一处理）——sleep
//     立即返回、当前待办处理完即退出；二次信号由 main 强杀。
//   - 验证失败重试一次（每待办至多两个会话），仍失败则记录并放行，等待下一轮
//     检测（标签未变意味着待办仍在列表里，循环天然重试）。
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"assistant/internal/claudecfg"
	"assistant/internal/status"
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
	// PrepareIssue 为 Issue 会话建基线 detach worktree（/tmp 隔离工作区，与 PR
	// 会话同口径）；未传时 Issue 会话退回在宿主检出内运行
	PrepareIssue   func(worktreeDir string) (string, error)
	RemoveWorktree func(dir string) error
	RunSession     func(request SessionRequest) SessionOutcome
	// 运行态上报（daemon API / MCP 状态查询用；均可为 nil）
	// OnQueue 每轮检测后上报当前待办清单（含空清单：队列已清空）
	OnQueue func(items []WorkItem)
	// OnStart 会话开始前上报（进入活跃列表）
	OnStart func(item WorkItem)
	// OnFinish 会话结束后上报（移出活跃列表并归档结果）
	OnFinish func(item WorkItem, outcome SessionOutcome)
}

// SessionRequest 是一次会话的输入：待办上下文（稳定会话 ID/标题用）+ 起始
// 提示词 + 工作目录；由 loop 组装、宿主注入执行细节。
type SessionRequest struct {
	Item    WorkItem
	HeadSHA string
	Prompt  string
	Cwd     string
	// OnProgress 折叠 stream-json 的实时进度（assistant 文本与工具调用）
	OnProgress func(string)
}

// RunAll 监督多个仓库的常驻循环：各自独立加锁（锁在各自检出内），任一循环
// 失败即取消其余循环并聚合返回；ctx 取消（首个 SIGINT/SIGTERM 由 main 触发）
// 时所有循环处理完当前待办后优雅退出。
func RunAll(ctx context.Context, depsList []Deps) error {
	if len(depsList) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var waitGroup sync.WaitGroup
	errorsChannel := make(chan error, len(depsList))
	for _, deps := range depsList {
		waitGroup.Add(1)
		go func(deps Deps) {
			defer waitGroup.Done()
			if err := RunLoop(ctx, deps); err != nil {
				errorsChannel <- err
				cancel()
			}
		}(deps)
	}
	waitGroup.Wait()
	close(errorsChannel)
	var runErrors []error
	for err := range errorsChannel {
		runErrors = append(runErrors, err)
	}
	return errors.Join(runErrors...)
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

// ctxDone 返回 ctx 是否已取消（优雅退出信号由 main 统一处理）。
func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// RunLoop 是常驻主循环（部署形态）。退出由 ctx 取消驱动：当前待办处理完即
// 退出，不中断进行中的会话。
func RunLoop(ctx context.Context, deps Deps) error {
	config := deps.Config

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
		case <-ctx.Done():
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

	// settled 是请求唯一键守卫（本进程内）：键为 kind#number，一个键同时只允许
	// 一个未完成请求；处理完成后置位，直到标签被 sync 收敛（键从待办列表消失）
	// 才解除——连续 @ai / /review 不会重复拉起同一待办。
	settled := map[string]bool{}

	for !ctxDone(ctx) {
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
		// 请求唯一键守卫：已在本进程内处理过（review 已提交/triage 已移除）但
		// 标签尚未被 sync 收敛的待办不再重复拉起——连续 @ai / /review 只会
		// 形成一个未完成的请求。标签消失（sync 收敛）后自动解除守卫，之后
		// 的新请求可以重新入队。
		work = pruneSettled(work, settled)
		if deps.OnQueue != nil {
			deps.OnQueue(append([]WorkItem(nil), work...))
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
		// 同一待办同一时刻至多一个会话
		var wg sync.WaitGroup
		var next int
		var mutex sync.Mutex
		worker := func() {
			defer wg.Done()
			for !ctxDone(ctx) {
				mutex.Lock()
				if next >= len(work) {
					mutex.Unlock()
					return
				}
				index := next
				next++
				mutex.Unlock()
				result := ProcessItem(ctx, deps, work[index])
				if result.Settled {
					mutex.Lock()
					settled[work[index].key()] = true
					mutex.Unlock()
				}
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
	settingsNote := "<临时独立配置：env 时长/输出上限 + 只读权限放行"
	if config.ProviderName != "" {
		settingsNote += " + provider " + config.ProviderName
	}
	settingsNote += ">"
	mcpNote := filepath.Join(deps.RepoDir, ".mcp.json")
	if _, _, mcpServers := config.Provider.Counts(); mcpServers > 0 {
		mcpNote = fmt.Sprintf("<临时合并配置：.mcp.json + provider %s 的 %d 个 server>",
			config.ProviderName, mcpServers)
	}
	// 演练时 head 尚未取到（真实运行时以 head 为锚点派生 ID），这里用占位
	sessionID := SessionID(config.Host, config.Repository.FullName(), item.Kind, item.Number, "<head>")
	transcript := claudecfg.TranscriptPath(config.SessionDir, config.SessionProject, sessionID)
	sessionMode := "--session-id " + sessionID + "（同一待办重试自动改 --resume 续接）"
	if transcript != "" {
		if _, err := os.Stat(transcript); err == nil {
			sessionMode = "--resume " + sessionID
		}
	}
	claudeArgs := []string{
		"--permission-mode auto",
		"--autocompact auto",
		"--output-format stream-json",
		"--verbose",
		"--strict-mcp-config",
		"--mcp-config " + mcpNote,
		"--settings " + settingsNote,
		"--setting-sources " + claudecfg.SettingSources,
		sessionMode,
		"--name " + SessionTitle(item.Kind, config.Repository.FullName(), item.Number, ""),
		fmt.Sprintf("--max-turns %d", MaxTurns),
	}
	if config.SessionProject != "" {
		claudeArgs = append(claudeArgs,
			fmt.Sprintf("CLAUDE_CONFIG_DIR=%s CLAUDE_CODE_PROJECT_DIR_NAME=%s（文本记录：%s）",
				config.SessionDir, config.SessionProject, transcript))
	}
	if _, err := os.Stat(filepath.Join(deps.RepoDir, ".assistant", "review.md")); err == nil {
		claudeArgs = append(claudeArgs, "--append-system-prompt <项目约定 .assistant/review.md>")
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
			"# 未完成：本轮放行，下一轮检测再处理（同一请求只拉起一个会话）",
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
		fmt.Sprintf("# 完成判定：%s 标签已从该 Issue 移除", status.LabelTriage),
		curl(fmt.Sprintf("/issues/%d/labels", item.Number)),
		"# 未完成：本轮放行，下一轮检测再处理（同一请求只拉起一个会话）",
	}
}

// pruneSettled 过滤掉已处理但仍留在待办列表里的条目（请求标签尚未被 sync
// 收敛），并清除已从列表消失的键（请求已收敛或关闭）——之后的重新请求可以
// 再次入队。
func pruneSettled(work []WorkItem, settled map[string]bool) []WorkItem {
	present := make(map[string]bool, len(work))
	filtered := work[:0]
	for _, item := range work {
		key := item.key()
		present[key] = true
		if settled[key] {
			continue
		}
		filtered = append(filtered, item)
	}
	for key := range settled {
		if !present[key] {
			delete(settled, key)
		}
	}
	return filtered
}

// ConfigPreview 返回生效调度配置的只读摘要（令牌掩码），供 run --dry-run
// 预览「当前实际用的是什么配置」。
func ConfigPreview(config Config, repoDir string) []string {
	session := "claude=" + config.ClaudeBin
	if config.DockerImage != "" {
		session = "docker=" + config.DockerImage
		if config.DockerNetwork != "" {
			session += "（network=" + config.DockerNetwork + "）"
		}
	}
	model := config.Model
	if model == "" {
		model = "账号默认"
	}
	return []string{
		fmt.Sprintf("配置预览：host=%s repo=%s reviewer=%s", config.Host, config.Repository.FullName(), config.Reviewer),
		fmt.Sprintf("  认证：令牌=%s 来源=--token/GITEA_ACCESS_TOKEN", maskSecret(config.AccessToken)),
		fmt.Sprintf("  会话：model=%s %s", model, session),
		fmt.Sprintf("  节奏：interval=%s timeout=%s concurrency=%d", config.Interval, config.SessionTimeout, config.Concurrency),
		fmt.Sprintf("  路径：repo_dir=%s worktree_root=%s log_dir=%s lock=%s",
			repoDir, config.WorktreeRoot, config.LogDir, config.LockFile),
		fmt.Sprintf("  基线：base_branch=%s sync_mirror=%t", config.BaseBranch, config.SyncMirror),
	}
}

// maskSecret 只保留前后各 4 位；过短时全掩码。
func maskSecret(secret string) string {
	if secret == "" {
		return "(空)"
	}
	if len(secret) <= 8 {
		return "***"
	}
	return secret[:4] + "***" + secret[len(secret)-4:]
}

// DryRunPass 是 run 的只读演练：先打印生效配置，再按同一检测口径列出将执行
// 的待办，逐条给出将执行的具体命令（只打印不执行）——不建 worktree、不起会话、
// 不写待办日志。
func DryRunPass(ctx context.Context, deps Deps) error {
	config := deps.Config
	for _, line := range ConfigPreview(deps.Config, deps.RepoDir) {
		deps.Log(line)
	}
	deps.Log("")
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
	TranscriptPath    string   `json:"transcriptPath,omitempty"`
	Resumed           bool     `json:"resumed,omitempty"`
	Result            string   `json:"result"`
	Errors            []string `json:"errors"`
	PermissionDenials int      `json:"permissionDenials"`
}

// ProcessResult 是一次处理的走向：Settled 表示已产出可验证的完成动作
// （review 已提交 / triage 标签已移除），该请求已满足；在标签被 sync 收敛前
// 不应重复拉起。
type ProcessResult struct {
	Settled bool
}

// ProcessItem 处理单个待办（主循环逐项调用；review/triage 一次性命令也走这里）。
// 会话进度两路落点：控制台实时显示基础进度，完整明细实时写待办日志。
// 每个待办每次只起一个会话：处理完即放行等待下一轮检测，同一请求的重复
// 信号（连续 @ai / /review）由 RunLoop 的 settled 唯一键守卫吸收。
func ProcessItem(ctx context.Context, deps Deps, item WorkItem) ProcessResult {
	config := deps.Config
	tag := item.key()
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

	switch {
	case item.Kind == KindPull:
		worktreeDir = filepath.Join(config.WorktreeRoot, fmt.Sprintf("pr-%d", item.Number))
		sha, err := deps.PrepareWorktree(item.Number)
		if err != nil {
			deps.Log(fmt.Sprintf("%s 处理异常：%v", tag, err))
			appendLog(fmt.Sprintf("[error] %v\n", err))
			return ProcessResult{}
		}
		headSHA = sha
		cwd = worktreeDir
		deps.Log(fmt.Sprintf("%s worktree=%s head=%s", tag, worktreeDir, headSHA))
	case deps.PrepareIssue != nil:
		// Issue 会话同样在 /tmp 的 detach worktree 里跑：宿主检出只做 fetch
		worktreeDir = filepath.Join(config.WorktreeRoot, fmt.Sprintf("issue-%d", item.Number))
		sha, err := deps.PrepareIssue(worktreeDir)
		if err != nil {
			deps.Log(fmt.Sprintf("%s 处理异常：%v", tag, err))
			appendLog(fmt.Sprintf("[error] %v\n", err))
			return ProcessResult{}
		}
		cwd = worktreeDir
		deps.Log(fmt.Sprintf("%s worktree=%s head=%s", tag, worktreeDir, sha))
	}
	prompt := deps.BuildPrompt(item.Kind, item.Number, PromptContext{Title: item.Title, HeadSHA: headSHA})
	appendLog("[prompt] " + strings.ReplaceAll(prompt, "\n", " ⏎ ") + "\n")

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
	startedAt := time.Now()
	if deps.OnStart != nil {
		deps.OnStart(item)
	}
	outcome := deps.RunSession(SessionRequest{
		Item:       item,
		HeadSHA:    headSHA,
		Prompt:     prompt,
		Cwd:        cwd,
		OnProgress: onProgress,
	})
	if deps.OnFinish != nil {
		deps.OnFinish(item, outcome)
	}
	record, err := json.Marshal(attemptRecord{
		Attempt:           1,
		StartedAt:         startedAt.UnixMilli(),
		Subtype:           outcome.Subtype,
		IsError:           outcome.IsError,
		NumTurns:          outcome.NumTurns,
		CostUSD:           outcome.CostUSD,
		DurationMS:        outcome.DurationMS,
		SessionID:         outcome.SessionID,
		TranscriptPath:    outcome.TranscriptPath,
		Resumed:           outcome.Resumed,
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
		return ProcessResult{}
	}
	appendLog(fmt.Sprintf("[verify] completed=%t headMoved=%t reason=%s\n",
		verdict.completed, verdict.headMoved, verdict.reason))
	if verdict.completed {
		deps.Log(fmt.Sprintf("%s 完成（%s）", tag, verdict.reason))
		return ProcessResult{Settled: true}
	}
	// 作者在会话期间推送 ⇒ 评审锚定的旧 head 已作废：直接放行，下一轮以新
	// head 重开（会话锚定的 head 记录在待办日志）
	if verdict.headMoved {
		deps.Log(fmt.Sprintf("%s %s；本轮放行，下一轮以新 head 重开", tag, verdict.reason))
		return ProcessResult{}
	}
	deps.Log(fmt.Sprintf("%s 本轮未完成，放行等待下一轮检测", tag))
	return ProcessResult{}
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
