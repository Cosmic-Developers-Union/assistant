// 主循环：检测待办 → 逐项（prepare → 会话 → 验证 → 清理）→ 空闲轮询。
//
//   - 单飞：同一时刻至多一个评审会话；lockfile 记 PID，宿主 PID 存活时拒绝
//     启动第二实例，死 PID（上次崩溃残留）自动接管。
//   - 优雅退出：ctx 取消（首个 SIGINT/SIGTERM，由 main 统一处理）——sleep
//     立即返回、当前待办处理完即退出；二次信号由 main 强杀。
//   - 验证失败重试一次（每待办至多两个会话），仍失败则记录并放行，等待下一轮
//     检测（标签未变意味着待办仍在列表里，循环天然重试）。
//   - 双通道守卫（guardState）：请求类通道（review 请求 / 分诊标签）settled
//     吸收同一请求的重复信号，mention 通道水位线吸收同一消息的重复信号、放行
//     新消息（追问轮）——两类行为分离，见 detect.go 头注释与 selectDispatch。
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
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// Deps 是主循环的依赖：CLI 注入真实实现，测试注入桩。
//
// 日志三级（default/verbose/debug，见 internal/logcfg）：
//   - Log = default（What happened）：状态、结果、警告、错误——启动横幅、
//     会话结果、完成判定、失败原因；
//   - LogVerbose = verbose（What is happening）：步骤、目标、外部调用——
//     worktree 准备、镜像同步 sha、检测清单、工具调用时间线；
//   - LogDebug = debug（Why is it happening）：内部决策与诊断——互斥跳过、
//     head 钉定、重试分支、配置细节。未传时降级为 Log（行为不变）。
type Deps struct {
	Config      Config
	API         API
	RepoDir     string
	Log         func(string)
	LogVerbose  func(string)
	LogDebug    func(string)
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
	// FollowUpMessages 返回 since 之后他人（非 assistant 账号）在条目上的新评论；
	// 未传时跳过「确认 ai 读到后续消息」的追问轮
	FollowUpMessages func(ctx context.Context, item WorkItem, since time.Time) ([]string, error)
	// PostFollowUpNote 给条目发一条系统评论（说明 ai 正在读取后续消息）；
	// 未传时跳过
	PostFollowUpNote func(ctx context.Context, item WorkItem, count int) error
	// 运行态上报（daemon API / MCP 状态查询用；均可为 nil）
	// OnQueue 每轮检测后上报当前待办清单（含空清单：队列已清空）
	OnQueue func(items []WorkItem)
	// OnStart 会话开始前上报（进入活跃列表）；返回 false 表示该待办已被互斥
	// 拒绝（同一待办同一时刻只允许一个会话），ProcessItem 直接放行
	OnStart func(item WorkItem) bool
	// OnFinish 会话结束后上报（移出活跃列表并归档结果）
	OnFinish func(item WorkItem, outcome SessionOutcome)
}

// logVerbose 是 verbose 级日志（What is happening）；未注入时降级为 Log。
func (d Deps) logVerbose(line string) {
	if d.LogVerbose != nil {
		d.LogVerbose(line)
		return
	}
	d.Log(line)
}

// logDebug 是 debug 级日志（Why is it happening）；未注入时降级为 Log。
func (d Deps) logDebug(line string) {
	if d.LogDebug != nil {
		d.LogDebug(line)
		return
	}
	d.Log(line)
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

	// 启动横幅：一眼确认「谁在跑、写哪里、按什么规则」
	if deps.CurrentLogin != nil {
		account, err := deps.CurrentLogin(ctx)
		if err != nil {
			deps.Log(fmt.Sprintf("账户查询失败：%v", err))
		} else {
			deps.Log(
				fmt.Sprintf("dispatcher 启动：host=%s repo=%s 账户=@%s reviewer=%s 并发=%d 检测间隔=%dms",
					config.Host, config.Repository.FullName(), account, config.Reviewer, config.Concurrency, mentionPollInterval(config.Interval).Milliseconds()),
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
			fmt.Sprintf("dispatcher 启动：host=%s repo=%s reviewer=%s 并发=%d 检测间隔=%dms",
				config.Host, config.Repository.FullName(), config.Reviewer, config.Concurrency, mentionPollInterval(config.Interval).Milliseconds()),
		)
	}
	model := ""
	if config.Model != "" {
		model = " model=" + config.Model
	}
	deps.logVerbose(fmt.Sprintf(
		"  会话：claude=%s%s timeout=%dms（%s）",
		config.ClaudeBin, model, config.SessionTimeout.Milliseconds(), humanDuration(config.SessionTimeout),
	))
	deps.logVerbose(fmt.Sprintf("  落点：logs=%s worktrees=%s lock=%s", config.LogDir, config.WorktreeRoot, config.LockFile))
	mirror := "关"
	if config.SyncMirror {
		mirror = "开"
	}
	deps.logVerbose(fmt.Sprintf(
		"  基线：origin/%s（镜像同步%s） 漂移规则：head 推进即本轮作废",
		config.BaseBranch, mirror,
	))

	// guards 是双通道守卫（本进程内，键 kind#number）。请求与 mention 是两类
	// 行为，守卫语义随之分离（完整说明见 detect.go 头注释）：
	//   - 请求类通道（review 请求 / Issue 分诊标签）settled：处理后压制，请求
	//     出清才解除——吸收同一请求的重复信号；
	//   - mention 通道 handled（水位线）：记录最后回应时刻，只吸收同一消息的
	//     重复信号，水位线后的他人新评论会以追问轮重新触发。
	guards := newGuardState()

	// 主循环 = 高频 mention 检测 + 常驻 worker 池，两个 loop 各司其职：
	//   - detector：mention/ticket 检测，默认 5s 一轮（run.yaml interval 可调），
	//     不做任何 sync——它只该回答「有没有新请求」；
	//   - worker：消费待办并起 claude 会话。镜像同步在**每次起会话前**执行
	//     （ProcessItem 的 PrepareWorktree 之前），保证评审基线新鲜，且不占用
	//     检测 loop 的频率。
	// 此前检测 loop 内嵌镜像同步且频率 30s：一是 @ 响应慢（最坏等一整个
	// interval + 会话 barrier），二是 sync 失败会卡住检测。
	workChannel := make(chan WorkItem)
	// inFlight 记录在跑/排队中的待办（防同一待办被重复派发）；会话结束即清理
	inFlight := map[string]bool{}
	var dispatchMutex sync.Mutex
	var workerWG sync.WaitGroup
	worker := func() {
		for item := range workChannel {
			result := ProcessItem(ctx, deps, item)
			// 会话结束即出队：下一轮检测若仍有 mention（用户追加了消息）可以
			// 重新入队；守卫按结果更新（Settled 置 settled，Responded 推进
			// mention 水位线）
			dispatchMutex.Lock()
			delete(inFlight, item.key())
			dispatchMutex.Unlock()
			guards.apply(item.key(), result, time.Now())
		}
	}
	for range config.Concurrency {
		workerWG.Go(worker)
	}
	defer close(workChannel)

	mentionInterval := mentionPollInterval(config.Interval)
	detect := func() {
		work, err := ListWork(ctx, deps.API, config.Repository, config.Reviewer)
		if err != nil {
			deps.Log(fmt.Sprintf("检测失败：%v（%dms 后重试）", err, mentionInterval.Milliseconds()))
			return
		}
		// 守卫解除先行：条目从各自清单消失（标签收敛 / mention 条目关闭或
		// mention 移除）后，同键的新请求可以重新入队
		guards.release(work, deps.logDebug)
		// 逐条目按通道判定派发模式。已回应 mention 条目的新评论细查可能发起
		// Gitea 调用，放在互斥区外，不阻塞 worker 的 inFlight 登记
		var candidates []WorkItem
		for _, item := range work {
			if dispatch, ok := deps.selectDispatch(ctx, guards, item); ok {
				candidates = append(candidates, dispatch)
			}
		}
		dispatchMutex.Lock()
		var queue []WorkItem
		for _, item := range candidates {
			if inFlight[item.key()] {
				continue
			}
			queue = append(queue, item)
			inFlight[item.key()] = true
		}
		dispatchMutex.Unlock()
		// OnQueue 上报「尚未派发」的清单（状态 API 的队列视图）
		if deps.OnQueue != nil {
			deps.OnQueue(queue)
		}
		if len(queue) == 0 {
			return
		}
		labels := make([]string, 0, len(queue))
		for _, item := range queue {
			labels = append(labels, item.label())
		}
		deps.Log(fmt.Sprintf("检测到 %d 个待办（并发 %d）：%s", len(queue), config.Concurrency, strings.Join(labels, " ")))
		for _, item := range queue {
			select {
			case workChannel <- item:
			case <-ctx.Done():
				return
			}
		}
	}

	detect()
	detectorTicker := time.NewTicker(mentionInterval)
	defer detectorTicker.Stop()
	for !ctxDone(ctx) {
		select {
		case <-ctx.Done():
		case <-detectorTicker.C:
			detect()
		}
	}
	// 退出：detector 停止后等在跑的会话处理完（channel 已 close，worker 自然
	// 收尾）
	workerWG.Wait()
	deps.Log("已退出主循环")
	return nil
}

// mentionPollInterval 是 mention 检测的轮询间隔：默认 5s（@ 触发要高频）；
// 配置的 interval 小于该值时取 interval（尊重更激进的配置），大于时取
// interval 的五分之一（下限 5s），保证检测始终快于旧节奏。
func mentionPollInterval(configured time.Duration) time.Duration {
	const base = 5 * time.Second
	if configured <= 0 {
		return base
	}
	if configured <= base {
		return configured
	}
	if configured/5 < base {
		return base
	}
	return configured / 5
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
	mcpNote := "<临时会话配置：assistant 注入的 gitea server"
	if _, _, mcpServers := config.Provider.Counts(); mcpServers > 0 {
		mcpNote += fmt.Sprintf(" + provider %s 的 %d 个 server", config.ProviderName, mcpServers)
	}
	if _, err := os.Stat(filepath.Join(deps.RepoDir, ".mcp.json")); err == nil {
		mcpNote += " + 仓库 .mcp.json 的其它 server"
	}
	mcpNote += ">"
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
			fmt.Sprintf("rm -rf %s/.claude   # PR 自带的评审规则一律不生效", worktreeDir),
			fmt.Sprintf("{ test -d %s/.claude && cp -r %s/.claude %s/.claude; } || true   # 基线标准（没有则跳过：settings/MCP/协议由 assistant 注入）",
				deps.RepoDir, deps.RepoDir, worktreeDir),
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
	// 会话以该令牌身份提交 review（sessionCredentialEnv 显式注入），配置来源
	// 决定会话内 MCP/daemon 读哪份 config.json
	configSource := config.ConfigPath
	if configSource == "" {
		configSource = "无（环境变量单实例模式）"
	}
	return []string{
		fmt.Sprintf("配置预览：host=%s repo=%s reviewer=%s", config.Host, config.Repository.FullName(), config.Reviewer),
		fmt.Sprintf("  认证：令牌=%s 来源=--token/GITEA_ACCESS_TOKEN（会话继承同身份）", maskSecret(config.AccessToken)),
		fmt.Sprintf("  配置：config=%s", configSource),
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
	work, err := ListWork(ctx, deps.API, config.Repository, config.Reviewer)
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

// ProcessResult 是一次处理的走向：
//   - Settled：请求类通道已产出可验证的完成动作（review 已提交 / triage 标签
//     已移除），该请求已满足；在请求出清前不应重复拉起。
//   - Responded：mention 通道已回应（会话无错误收尾），mention 水位线推进到
//     此刻；清单里水位线之后的他人新评论才会再次触发。
type ProcessResult struct {
	Settled   bool
	Responded bool
}

// guardState 是检测循环与 worker 之间的双通道守卫（键 kind#number，本进程内）。
// 自带互斥：detector 读写、worker 收尾写全部在锁内，杜绝并发 map 访问。
//
//   - settled：请求类通道（review 请求 / Issue 分诊标签）——已可验证完成的
//     请求在请求出清前压制，防止同一请求重复拉起会话；
//   - handled：mention 通道——最后回应时刻（水位线）。mention 条目会持续留在
//     mentioned_by 清单里，水位线只吸收「同一消息的重复信号」，他人新评论仍会
//     重新触发；这与请求类通道 settled 的「请求已满足」语义不同，不可混用。
type guardState struct {
	mutex   sync.Mutex
	settled map[string]bool
	handled map[string]time.Time
}

func newGuardState() *guardState {
	return &guardState{settled: map[string]bool{}, handled: map[string]time.Time{}}
}

// get 返回键的守卫状态：是否已 settled、mention 水位线（零值 = 从未回应）。
func (g *guardState) get(key string) (settled bool, handled time.Time) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.settled[key], g.handled[key]
}

// apply 把一次处理结果记入守卫：Settled 置请求类通道压制，Responded 推进
// mention 水位线（at 为回应完成时刻）。
func (g *guardState) apply(key string, result ProcessResult, at time.Time) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if result.Settled {
		g.settled[key] = true
	}
	if result.Responded {
		g.handled[key] = at
	}
}

// release 清除已从各自清单消失的键：请求类信号（review 请求 / Issue 分诊标签）
// 收敛（清单里不再出现）解除 settled；mention 条目关闭或 mention 被移除后清除
// 水位线——重新请求或再次 mention 视为全新请求。状态变化走 debug 日志
// （Why is it happening）。
func (g *guardState) release(work []WorkItem, debug func(string)) {
	requested := map[string]bool{}
	mentioned := map[string]bool{}
	for _, item := range work {
		if item.Requested || item.Labeled {
			requested[item.key()] = true
		}
		if item.Mention {
			mentioned[item.key()] = true
		}
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	for key := range g.settled {
		if !requested[key] {
			delete(g.settled, key)
			debug(fmt.Sprintf("%s 请求已出清（reviewer 已回应/标签收敛），settled 守卫解除（新请求可入队）", key))
		}
	}
	for key := range g.handled {
		if !mentioned[key] {
			delete(g.handled, key)
			debug(fmt.Sprintf("%s 已离开 mention 清单，水位线清除（重新 mention 视为全新请求）", key))
		}
	}
}

// clockSkewTolerance 是 mention 水位线预检的时钟偏差余量：水位线取本地时钟，
// 条目 Updated 取 Gitea 服务端时钟，预检按水位线前推该余量，避免时钟偏差漏掉
// 真实新评论；是否真有新消息仍以评论列表细查为准。
const clockSkewTolerance = 2 * time.Minute

// selectDispatch 按通道判定单个条目本轮是否派发、以何种模式派发（各通道行为
// 的完整定义见 detect.go 头注释）。ok=false 表示所有通道都不需要动。
//
//   - 请求类通道（review 请求、Issue 分诊标签）：请求在清单且未 settled →
//     全量会话；已 settled 的等待请求出清（reviewer 回应当前 head / 标签
//     收敛），不重复拉起。
//   - mention 通道：从未回应 → 全量会话；已回应 → 仅当条目在水位线后有新动态
//     （Updated 预检把常态开销压到零）且确有他人新评论时，以追问轮（同一会话
//     续聊）再次入队，不重跑全量协议。
//
// 同一条目多路同时命中时请求类通道优先：全量会话覆盖追问诉求，会话后的追问轮
// 会消化新消息。
func (d Deps) selectDispatch(ctx context.Context, guards *guardState, item WorkItem) (WorkItem, bool) {
	settled, handled := guards.get(item.key())
	if (item.Requested || item.Labeled) && !settled {
		return item, true
	}
	if !item.Mention {
		return WorkItem{}, false
	}
	if handled.IsZero() {
		return item, true
	}
	if !item.Updated.After(handled.Add(-clockSkewTolerance)) {
		return WorkItem{}, false
	}
	if d.FollowUpMessages == nil {
		return WorkItem{}, false
	}
	messages, err := d.FollowUpMessages(ctx, item, handled)
	if err != nil {
		d.Log(fmt.Sprintf("%s 新消息检查失败：%v（下一轮重试）", item.label(), err))
		return WorkItem{}, false
	}
	if len(messages) == 0 {
		// 有动态但全是自身活动（自己的评论、标签变更）：不重新入队
		d.logDebug(fmt.Sprintf("%s 水位线后无他人新评论，不重新入队", item.label()))
		return WorkItem{}, false
	}
	d.logVerbose(fmt.Sprintf("%s 水位线后 %d 条新消息，以追问轮入队（同一会话续聊）", item.label(), len(messages)))
	item.FollowUp = true
	item.Since = handled
	return item, true
}

// ProcessItem 处理单个待办（主循环逐项调用；review/triage 一次性命令也走这里）。
// 会话进度两路落点：控制台实时显示基础进度，完整明细实时写待办日志。
// 每个待办每次只起一个会话：处理完即放行等待下一轮检测，同一请求的重复信号
// 由主循环的双通道守卫吸收（标签 settled / mention 水位线，见 guardState）。
func ProcessItem(ctx context.Context, deps Deps, item WorkItem) ProcessResult {
	config := deps.Config
	tag := item.label()
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

	// 镜像同步在每次起会话前执行（fail-closed）：评审基线必须新鲜，且同步
	// 只属于「要起会话」这条路径，不占用检测 loop 的频率。检出是评审标准
	// （.claude/）与分诊的数据源，与其带着陈旧基线开会话，不如跳过本条。
	if deps.SyncMirror != nil {
		sha, err := deps.SyncMirror()
		if err != nil {
			deps.Log(fmt.Sprintf("%s 镜像同步失败：%v（跳过本条，下一轮重试）", tag, err))
			appendLog(fmt.Sprintf("[error] 镜像同步失败：%v\n", err))
			return ProcessResult{}
		}
		deps.logVerbose(fmt.Sprintf("%s 镜像同步：%s @ %s", tag, config.BaseBranch, sha))
	}

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
		deps.logVerbose(fmt.Sprintf("%s worktree=%s head=%s", tag, worktreeDir, headSHA))
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
		deps.logVerbose(fmt.Sprintf("%s worktree=%s head=%s", tag, worktreeDir, sha))
	}
	// 追问轮（仅 mention 通道派发）：不重跑全量协议，把水位线之后的他人新
	// 消息喂给同一会话续聊
	if item.FollowUp {
		return deps.processFollowUp(ctx, item, cwd, headSHA, appendLog)
	}
	prompt := deps.BuildPrompt(item.Kind, item.Number, PromptContext{Title: item.Title, HeadSHA: headSHA})
	appendLog("[prompt] " + strings.ReplaceAll(prompt, "\n", " ⏎ ") + "\n")
	// --debug：起始提示词全文上控制台（Why is it happening：会话被要求做什么）
	deps.logDebug(fmt.Sprintf("%s prompt：%s", tag, strings.ReplaceAll(prompt, "\n", " ⏎ ")))

	startedAt := time.Now()
	outcome, started := deps.runSessionRecorded(item, SessionRequest{
		Item:    item,
		HeadSHA: headSHA,
		Prompt:  prompt,
		Cwd:     cwd,
	}, appendLog, startedAt)
	if !started {
		// 同一待办已有在跑的会话（互斥守卫）：本轮放行，等下一轮检测；
		// worktree 由 defer 统一清理
		return ProcessResult{}
	}
	responded := !outcome.IsError

	verdict, err := verifyItem(ctx, deps, item, startedAt, headSHA)
	if err != nil {
		deps.Log(fmt.Sprintf("%s 处理异常：%v", tag, err))
		appendLog(fmt.Sprintf("[error] %v\n", err))
		return ProcessResult{Responded: responded}
	}
	appendLog(fmt.Sprintf("[verify] completed=%t headMoved=%t reason=%s\n",
		verdict.completed, verdict.headMoved, verdict.reason))
	if verdict.completed {
		deps.Log(fmt.Sprintf("%s 完成（%s）", tag, verdict.reason))
		// 完成不等于收工：会话期间或验证之后用户可能又留了新消息。追问轮保证
		// 「ai 回复前必须读到消息」——同一会话 --resume 续聊，直到没有未读
		deps.followUp(ctx, item, tag, startedAt, cwd, appendLog)
		return ProcessResult{Settled: true, Responded: responded}
	}
	// 作者在会话期间推送 ⇒ 评审锚定的旧 head 已作废：直接放行，下一轮以新
	// head 重开（会话锚定的 head 记录在待办日志）
	if verdict.headMoved {
		deps.logDebug(fmt.Sprintf("%s %s；本轮放行，下一轮以新 head 重开", tag, verdict.reason))
		return ProcessResult{Responded: responded}
	}
	deps.logDebug(fmt.Sprintf("%s 本轮未完成，放行等待下一轮检测", tag))
	return ProcessResult{Responded: responded}
}

// runSessionRecorded 是「起一个会话」的公共路径（全量会话与追问轮共用）：
// sqlite 互斥守卫（OnStart）→ 会话（进度两路落日志：控制台基础进度 + 待办
// 日志全量明细）→ 结果上报（OnFinish）与待办日志记录。started=false 表示互斥
// 拒绝（同一待办已有在跑的会话），本轮放行。
func (d Deps) runSessionRecorded(item WorkItem, request SessionRequest, appendLog func(string), startedAt time.Time) (SessionOutcome, bool) {
	tag := item.label()
	if d.OnStart != nil && !d.OnStart(item) {
		d.logDebug(fmt.Sprintf("%s 已有会话在处理（互斥守卫），本轮放行", tag))
		return SessionOutcome{}, false
	}
	toolCalls := 0
	request.OnProgress = func(line string) {
		appendLog("[progress] " + line + "\n")
		switch {
		case strings.HasPrefix(line, "session="):
			d.Log(tag + " " + line)
		case strings.HasPrefix(line, "🔧 "):
			toolCalls++
			d.logVerbose(fmt.Sprintf("%s 🔧 #%d %s", tag, toolCalls, strings.TrimPrefix(line, "🔧 ")))
		case strings.HasPrefix(line, "[debug] "):
			// 会话内部的 debug 明细（完整命令行、注入 env、stream 事件、stderr
			// 尾部）：--debug 时上控制台（Why is it happening），其余级别只落
			// 待办日志
			d.logDebug(tag + " " + strings.TrimPrefix(line, "[debug] "))
		case strings.HasPrefix(line, "[debug]事件"):
			d.logDebug(tag + " " + line)
		}
	}
	outcome := d.RunSession(request)
	if d.OnFinish != nil {
		d.OnFinish(item, outcome)
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
	d.Log(fmt.Sprintf("%s 会话结束：%s turns=%d cost=$%.2f%s",
		tag, outcome.Subtype, outcome.NumTurns, outcome.CostUSD, denials))
	return outcome, true
}

// processFollowUp 是追问轮的执行路径（仅 mention 通道派发）：把水位线之后的
// 他人新消息喂给同一会话（RunSession 按会话记录自动 --resume 续聊），不重跑
// 全量协议，也没有标签式完成判定——会话无错误收尾即视为已回应，水位线由主
// 循环推进。会话期间又到达的新消息由 followUp 兜底续读。
func (d Deps) processFollowUp(ctx context.Context, item WorkItem, cwd string, headSHA string, appendLog func(string)) ProcessResult {
	tag := item.label()
	if d.FollowUpMessages == nil {
		d.Log(fmt.Sprintf("%s 未接入新消息检查，追问轮无法执行", tag))
		return ProcessResult{}
	}
	messages, err := d.FollowUpMessages(ctx, item, item.Since)
	if err != nil {
		d.Log(fmt.Sprintf("%s 新消息读取失败：%v（下一轮重试）", tag, err))
		appendLog(fmt.Sprintf("[error] %v\n", err))
		return ProcessResult{}
	}
	if len(messages) == 0 {
		// 检测与派发之间消息已被消化（如上一轮追问已覆盖）：仅推进水位线
		d.logDebug(fmt.Sprintf("%s 待回应消息已消失，仅推进水位线", tag))
		return ProcessResult{Responded: true}
	}
	d.Log(fmt.Sprintf("%s 有 %d 条新消息，追问轮（同一会话续聊）", tag, len(messages)))
	if d.PostFollowUpNote != nil {
		if err := d.PostFollowUpNote(ctx, item, len(messages)); err != nil {
			d.Log(fmt.Sprintf("%s 追问说明评论失败：%v", tag, err))
		}
	}
	for _, message := range messages {
		appendLog("[follow-up] " + strings.ReplaceAll(message, "\n", " ⏎ ") + "\n")
	}
	prompt := followUpPrompt(item.Number, messages)
	appendLog("[follow-up prompt] " + strings.ReplaceAll(prompt, "\n", " ⏎ ") + "\n")
	startedAt := time.Now()
	outcome, started := d.runSessionRecorded(item, SessionRequest{
		Item:    item,
		HeadSHA: headSHA,
		Prompt:  prompt,
		Cwd:     cwd,
	}, appendLog, startedAt)
	if !started {
		return ProcessResult{}
	}
	appendLog(fmt.Sprintf("[follow-up result] subtype=%s turns=%d is_error=%t\n",
		outcome.Subtype, outcome.NumTurns, outcome.IsError))
	if outcome.IsError {
		d.Log(fmt.Sprintf("%s 追问轮失败（%s），放行等待下一轮", tag, outcome.Subtype))
		return ProcessResult{}
	}
	// 会话期间可能又到了新消息：与全量会话同一兜底，从本轮起点续读
	d.followUp(ctx, item, tag, startedAt, cwd, appendLog)
	return ProcessResult{Responded: true}
}

// maxFollowUpRounds 是追问轮上限：每轮都会话 --resume 读取增量消息并回应，
// 超限放行（下一轮检测重新处理，防死循环）。
const maxFollowUpRounds = 3

// followUpPrompt 构造追问轮提示词：要求先完整阅读全部新消息，再按既定协议
// 续处理（回答问题、补充结论或调整判断）。全量会话后的兜底追问与检测循环
// 直派的追问轮共用。
func followUpPrompt(number int64, messages []string) string {
	return fmt.Sprintf(
		"你在处理 #%d 期间或之后，用户又留下了 %d 条新消息（已在下方原文给出）。"+
			"回复前必须先完整阅读这些消息；按消息内容继续处理本待办：回答问题、"+
			"补充结论或调整你此前的判断，仍按既定协议（评审/分诊）落结论。\n\n"+
			"新消息如下：\n%s",
		number, len(messages), strings.Join(messages, "\n\n---\n\n"))
}

// followUp 检查条目上 since 之后他人（非 assistant 自身账号）的新评论；有则
// 拉起追问会话（同一会话记录 --resume 续聊），让 ai 读到消息后再回复。评论
// 本身也写入待办日志，时间线完整。
func (d Deps) followUp(
	ctx context.Context,
	item WorkItem,
	tag string,
	since time.Time,
	cwd string,
	appendLog func(string),
) {
	if d.FollowUpMessages == nil {
		return
	}
	for round := 1; round <= maxFollowUpRounds; round++ {
		messages, err := d.FollowUpMessages(ctx, item, since)
		if err != nil {
			d.Log(fmt.Sprintf("%s 后续消息检查失败：%v", tag, err))
			return
		}
		if len(messages) == 0 {
			return
		}
		d.Log(fmt.Sprintf("%s 有 %d 条后续消息，追问轮 %d/%d（同一会话续聊）",
			tag, len(messages), round, maxFollowUpRounds))
		for _, message := range messages {
			appendLog("[follow-up] " + strings.ReplaceAll(message, "\n", " ⏎ ") + "\n")
		}
		if d.PostFollowUpNote != nil {
			if err := d.PostFollowUpNote(ctx, item, len(messages)); err != nil {
				d.Log(fmt.Sprintf("%s 追问说明评论失败：%v", tag, err))
			}
		}
		prompt := followUpPrompt(item.Number, messages)
		appendLog("[follow-up prompt] " + strings.ReplaceAll(prompt, "\n", " ⏎ ") + "\n")
		outcome := d.RunSession(SessionRequest{
			Item:   item,
			Prompt: prompt,
			Cwd:    cwd,
			OnProgress: func(line string) {
				appendLog("[follow-up progress] " + line + "\n")
			},
		})
		appendLog(fmt.Sprintf("[follow-up result] subtype=%s turns=%d is_error=%t\n",
			outcome.Subtype, outcome.NumTurns, outcome.IsError))
		if outcome.IsError {
			d.Log(fmt.Sprintf("%s 追问轮 %d 失败（%s），放行等待下一轮", tag, round, outcome.Subtype))
			return
		}
		since = time.Now()
	}
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
