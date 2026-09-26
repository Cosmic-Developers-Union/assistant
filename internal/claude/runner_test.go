package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// execRunner 是唯一启动真实子进程的地方，也是唯一该被真实驱动的地方：用 sh 造出
// 「正常退出」「非零退出」「无换行结尾」等既便宜又确定的场景，比伪造一个假的
// claude 二进制干净。信号升级路径用注入的钩子断言（真实信号在单测里没有可观察的
// 副作用）。

// requireShell 在缺少 sh 的平台上跳过（本项目的目标平台都有）。
func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("环境缺少 sh：%v", err)
	}
}

// collectLines 收集 onLine 交付的每一行。
func collectLines() (*[]string, func([]byte)) {
	lines := &[]string{}
	return lines, func(line []byte) { *lines = append(*lines, string(line)) }
}

// 正常退出：stdout 逐行交付，返回 nil。
func TestExecRunnerSuccess(t *testing.T) {
	requireShell(t)
	lines, onLine := collectLines()
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `printf '{"type":"result","is_error":false}\n{"type":"result","result":"ok"}\n'`},
	}, onLine)
	if err != nil {
		t.Fatalf("正常退出应为 nil：%v", err)
	}
	if len(*lines) != 2 {
		t.Fatalf("应交付 2 行：%v", *lines)
	}
	if !strings.Contains((*lines)[1], `"ok"`) {
		t.Errorf("第二行 = %q", (*lines)[1])
	}
}

// 末行没有换行符也必须交付：result 消息是最后一行，流被中断时常不带换行，丢了
// 它调用方就看不到会话结论。
func TestExecRunnerFlushesTrailingLineWithoutNewline(t *testing.T) {
	requireShell(t)
	lines, onLine := collectLines()
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `printf '{"type":"result","result":"tail"}'`},
	}, onLine)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], `"tail"`) {
		t.Fatalf("无换行结尾的末行必须交付：%v", *lines)
	}
}

// 超长单行（大 diff / base64）不被默认 64KiB 上限截断。直接驱动 scanLines：被测
// 对象是这个缓冲上限，不是整条 Run 路径。
func TestScanLinesAcceptsLongLines(t *testing.T) {
	requireShell(t)
	command := exec.Command("sh", "-c", `printf '%s' "$(head -c 200000 /dev/zero | tr '\0' 'x')"`)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	scanErr := scanLines(stdout, func(line []byte) { lines = append(lines, string(line)) })
	_ = command.Wait()
	if scanErr != nil {
		t.Fatalf("scanLines = %v", scanErr)
	}
	if len(lines) != 1 {
		t.Fatalf("应交付 1 行：%d 行", len(lines))
	}
	if len(lines[0]) != 200000 {
		t.Errorf("长行被截断：%d 字节", len(lines[0]))
	}
}

// 超长单行走完整 Run 路径同样不被截断，且进程能正常退出（缓冲上限生效）。
func TestExecRunnerAcceptsLongLines(t *testing.T) {
	requireShell(t)
	lines, onLine := collectLines()
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `printf '%s\n' "$(head -c 200000 /dev/zero | tr '\0' 'x')"`},
	}, onLine)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(*lines) != 1 {
		t.Fatalf("应交付 1 行：%d 行", len(*lines))
	}
	if len((*lines)[0]) != 200000 {
		t.Errorf("长行被截断：%d 字节", len((*lines)[0]))
	}
}

// 二进制缺失：错误带可读的语境（「启动 <bin>」），且不 panic。
func TestExecRunnerMissingBinary(t *testing.T) {
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "/nonexistent/definitely-not-claude",
		Args: []string{"-p", "x"},
	}, nil)
	if err == nil {
		t.Fatal("缺失二进制应报错")
	}
	if !strings.Contains(err.Error(), "启动") {
		t.Errorf("错误应说明启动失败：%v", err)
	}
}

// 非零退出：错误带退出码与 stderr 尾部——失败诊断的关键一行。
func TestExecRunnerNonZeroExitCarriesStderrTail(t *testing.T) {
	requireShell(t)
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `printf 'stdout line\n'; printf 'boom: config invalid' >&2; exit 3`},
	}, nil)
	if err == nil {
		t.Fatal("非零退出应报错")
	}
	if !strings.Contains(err.Error(), "退出码 3") {
		t.Errorf("错误应含退出码：%v", err)
	}
	if !strings.Contains(err.Error(), "boom: config invalid") {
		t.Errorf("错误应含 stderr 尾部：%v", err)
	}
}

// 非零退出但 stderr 为空（如 exit 1 无输出）：给出退出码即可，不产出空的「：」尾巴。
func TestExecRunnerNonZeroExitWithoutStderr(t *testing.T) {
	requireShell(t)
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", "exit 7"},
	}, nil)
	if err == nil {
		t.Fatal("非零退出应报错")
	}
	if !strings.Contains(err.Error(), "退出码 7") {
		t.Errorf("错误应含退出码：%v", err)
	}
	if strings.HasSuffix(err.Error(), "：") {
		t.Errorf("无 stderr 时不应留空尾巴：%v", err)
	}
}

// 被信号终止：报信号名而非「退出码 -1」（后者读不出原因）。
func TestExecRunnerSignaledExits(t *testing.T) {
	requireShell(t)
	// sh 自杀：-TERM 让自身收到 SIGTERM
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `kill -TERM $$; sleep 5`},
	}, nil)
	if err == nil {
		t.Fatal("被信号终止应报错")
	}
	if !strings.Contains(err.Error(), "信号") && !strings.Contains(err.Error(), "异常终止") {
		t.Errorf("错误应说明终止方式：%v", err)
	}
}

// 超时：终止钩子被调用，且 Run 及时返回（不等子进程自然睡醒）。
//
// terminate 用真实的 terminateProcess：若只记调用次数、让子进程继续活着，这条
// 测试就退化成了「等 sleep 结束」——它测的是终止动作而非「超时确实会终止」。
func TestExecRunnerTimeoutTerminates(t *testing.T) {
	requireShell(t)
	var terminated int32
	runner := execRunner{
		terminate: func(process *os.Process) error { terminated++; return terminateProcess(process) },
		kill:      func(process *os.Process) error { return process.Kill() },
	}
	// 子进程睡 30s，超时设 100ms
	start := time.Now()
	err := runner.Run(context.Background(), Spec{
		Bin:     "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 100 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("超时应报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误应说明超时：%v", err)
	}
	if terminated != 1 {
		t.Errorf("超时后应发 SIGTERM 一次：%d", terminated)
	}
	// 关键断言：Run 在终止后立刻返回，而不是挂到子进程睡醒。
	//
	// 这条断言守着一个真实缺陷：stderr 若非 *os.File，os/exec 会起 goroutine
	// 拷贝它，而 Wait() 要等那份拷贝读到 EOF——子进程把写端交给了后代时，
	// SIGTERM 之后仍要挂满整个 sleep。换成临时文件后 Wait 只等进程本身。
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时应立即返回而非等子进程睡醒，实际 %v", elapsed)
	}
}

// 容器形态超时：终止 docker 客户端之外还必须按名 `docker kill`（终止客户端不会
// 停掉容器）。
func TestExecRunnerTimeoutKillsContainer(t *testing.T) {
	requireShell(t)
	var killedContainer string
	runner := execRunner{
		// 真实终止进程，只把 killContainer 换成钩子——否则这条测试会挂在子进程
		// 的 sleep 上，既慢又测不出「超时后容器被按名终止」。
		terminate:     terminateProcess,
		kill:          func(process *os.Process) error { return process.Kill() },
		killContainer: func(name string) error { killedContainer = name; return nil },
	}
	err := runner.Run(context.Background(), Spec{
		Bin:       "sh",
		Args:      []string{"-c", "sleep 30"},
		Timeout:   50 * time.Millisecond,
		Container: "assistant-review-1",
	}, nil)
	if err == nil {
		t.Fatal("超时应报错")
	}
	if killedContainer != "assistant-review-1" {
		t.Errorf("容器未按名终止：%q", killedContainer)
	}
}

// 非容器形态不发 docker kill（不引入一次无谓的 exec）。
func TestExecRunnerTimeoutSkipsContainerKillWhenAbsent(t *testing.T) {
	requireShell(t)
	called := false
	runner := execRunner{
		terminate:     terminateProcess,
		kill:          func(process *os.Process) error { return process.Kill() },
		killContainer: func(string) error { called = true; return nil },
	}
	_ = runner.Run(context.Background(), Spec{
		Bin:     "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 50 * time.Millisecond,
	}, nil)
	if called {
		t.Error("非容器形态不应调用 docker kill")
	}
}

// ctx 取消：进程被终止、Run 及时返回错误，且**取消前已产出的行仍要交付**。
//
// 这是 daemon 的会话超时机制（它用 ctx 限时，Spec.Timeout 留 0）。两处都要守住：
// 静默成功会让一轮挂住的对话被当成空回复发出去；丢已产出的行则会丢掉结论。
func TestExecRunnerContextCancellation(t *testing.T) {
	requireShell(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	lines, onLine := collectLines()
	start := time.Now()
	err := NewExecRunner().Run(ctx, Spec{
		Bin: "sh",
		Args: []string{"-c",
			`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}\n'; sleep 30`},
	}, onLine)
	if err == nil {
		t.Fatal("ctx 取消后 Run 不应返回 nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ctx 取消应立即生效，实际 %v", elapsed)
	}
	if len(*lines) == 0 || !strings.Contains((*lines)[0], "working") {
		t.Errorf("取消前已产出的行应仍交付：%v", *lines)
	}
}

// onLine 为 nil 时只丢进度、不 panic（无进度订阅的路径）。
func TestExecRunnerNilOnLine(t *testing.T) {
	requireShell(t)
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `printf '{"a":1}\n'`},
	}, nil)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// Spec.Env 追加到父环境之上（不是替换）：CLI 需要 PATH 等基础变量。
func TestExecRunnerAppendsEnv(t *testing.T) {
	requireShell(t)
	lines, onLine := collectLines()
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", `printf '%s\n' "$ASSISTANT_TEST_MARKER"`},
		Env:  []string{"ASSISTANT_TEST_MARKER=hello"},
	}, onLine)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(*lines) != 1 || (*lines)[0] != "hello" {
		t.Errorf("注入的环境变量未生效：%v", *lines)
	}
}

// Spec.Dir 是子进程的工作目录。
func TestExecRunnerWorkingDirectory(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	lines, onLine := collectLines()
	if err := NewExecRunner().Run(context.Background(), Spec{
		Bin:  "sh",
		Args: []string{"-c", "pwd"},
		Dir:  dir,
	}, onLine); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(*lines) != 1 {
		t.Fatalf("应交付一行 cwd：%v", *lines)
	}
	// macOS 的 /tmp 是 /private/tmp 的符号链接，比较尾部即可
	if !strings.HasSuffix(strings.TrimSpace((*lines)[0]), dir[len(dir)-8:]) {
		t.Errorf("工作目录 = %q, want 尾部 %q", (*lines)[0], dir[len(dir)-8:])
	}
}

// readTail 只留尾部：整段 stderr 会淹掉日志。
func TestReadTailKeepsTail(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("0123456789ABCDEF"); err != nil {
		t.Fatal(err)
	}
	if got := readTail(file, 10); got != "6789ABCDEF" {
		t.Errorf("tail = %q, want 6789ABCDEF", got)
	}
	// 短于上限时全量返回
	if got := readTail(file, 100); got != "0123456789ABCDEF" {
		t.Errorf("tail = %q, want 全量", got)
	}
	// 空文件返回空串（不 panic、不返回垃圾）
	if _, err := file.WriteAt(make([]byte, 0), 0); err != nil {
		t.Fatal(err)
	}
	empty, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	if got := readTail(empty, 10); got != "" {
		t.Errorf("空文件 tail = %q, want 空", got)
	}
}

// describeExit 的分类：正退出码给数字。
func TestDescribeExitPositiveCode(t *testing.T) {
	if got := describeExit(nil, 3); got != "退出码 3" {
		t.Errorf("describeExit(3) = %q", got)
	}
}

// scanLines 把底层读取错误向上传递（不是静默当 EOF）。
func TestScanLinesPropagatesReadError(t *testing.T) {
	err := scanLines(errReader{}, nil)
	if err == nil {
		t.Fatal("读取错误应向上传递")
	}
	if !errors.Is(err, errBoom) {
		t.Errorf("错误链应保留：%v", err)
	}
}

// errBoom 是测试用的哨兵错误。
var errBoom = errors.New("boom")

// errReader 立刻返回错误（模拟管道读失败）。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

// terminateContainer 把容器名交给 killContainer 钩子（不给钩子时才自己 exec
// docker）。两条分支都不该碰进程环境——这里只验分派。
func TestTerminateContainerDispatchesToHook(t *testing.T) {
	var got string
	runner := execRunner{killContainer: func(name string) error { got = name; return nil }}

	runner.terminateContainer(Spec{Container: "assistant-review-1"})
	if got != "assistant-review-1" {
		t.Errorf("killContainer 收到 %q", got)
	}

	// 容器名为空：不调用钩子（非容器形态不引入一次无谓的 kill）
	got = ""
	runner.terminateContainer(Spec{})
	if got != "" {
		t.Errorf("容器名为空时不应调用 killContainer：%q", got)
	}
}

// 缺省 killContainer 真的执行 `docker kill <name>`。断言命令行的构造，用注入的
// exec 替身而不是改 PATH——改 PATH 是进程级状态，会与并发跑的其它测试互相干扰。
func TestTerminateContainerDefaultCommandShape(t *testing.T) {
	original := dockerKillCommand
	t.Cleanup(func() { dockerKillCommand = original })

	var recorded []string
	dockerKillCommand = func(name string, args ...string) error {
		recorded = append([]string{name}, args...)
		return nil
	}

	execRunner{}.terminateContainer(Spec{Container: "assistant-review-1"})
	if len(recorded) != 3 || recorded[0] != "docker" || recorded[1] != "kill" || recorded[2] != "assistant-review-1" {
		t.Errorf("docker 命令行 = %v, want [docker kill assistant-review-1]", recorded)
	}
}

// describeExit 的三条出口：正退出码给数字、被信号终止给信号名、其余说明异常终止。
//
// 前两条由 TestProcessSignaled 用真实进程覆盖；这里只补「非正退出码且拿不到信号
// 信息」的那条——用一个真实的、非 signaled 的退出状态（state.Sys() 是
// WaitStatus 但 Signaled 为假）驱动。
func TestDescribeExitAnomalousTermination(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 0")
	_ = command.Run()
	if command.ProcessState == nil {
		t.Skip("进程未产生退出状态")
	}
	// 退出码 0 传进来即「非正」分支；该状态未 signaled ⇒ 异常终止
	if got := describeExit(command.ProcessState, 0); got != "异常终止" {
		t.Errorf("describeExit(state, 0) = %q, want 异常终止", got)
	}
	// 正退出码优先给数字
	if got := describeExit(command.ProcessState, 4); got != "退出码 4" {
		t.Errorf("describeExit(state, 4) = %q", got)
	}
}

// 零值 execRunner（= NewExecRunner()）的缺省终止动作必须真的可用：terminate/kill
// 为 nil 时回退到 terminateProcess 与 Kill。这条覆盖 NewExecRunner 的真实形态——
// 前面的超时测试注入了钩子，跳过了回退分支。
func TestExecRunnerDefaultHooksTerminateForReal(t *testing.T) {
	requireShell(t)
	start := time.Now()
	err := NewExecRunner().Run(context.Background(), Spec{
		Bin:     "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 100 * time.Millisecond,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("Run = %v, want 超时", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("缺省终止动作未生效，等了 %v", elapsed)
	}
}

// 宽限期后的 SIGKILL 兜底：terminate 故意无效（模拟进程忽略 SIGTERM），kill 必须
// 在 KillGrace 后被调用。用极短的宽限期验证，不等真实的 5s。
func TestExecRunnerKillGraceEscalatesToKill(t *testing.T) {
	requireShell(t)
	original := KillGrace
	KillGrace = 100 * time.Millisecond
	t.Cleanup(func() { KillGrace = original })

	killed := make(chan struct{})
	runner := execRunner{
		// 忽略 SIGTERM：模拟进程不响应优雅终止
		terminate: func(*os.Process) error { return nil },
		kill: func(process *os.Process) error {
			close(killed)
			return process.Kill()
		},
	}
	err := runner.Run(context.Background(), Spec{
		Bin:     "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 50 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("超时应报错")
	}
	select {
	case <-killed:
	case <-time.After(3 * time.Second):
		t.Fatal("宽限期后未调用 SIGKILL 兜底")
	}
}

// describeExit 的三条出口：正退出码给数字、被信号终止给信号名、其余说明异常终止。
//
// 前两条由 TestProcessSignaled 用真实进程覆盖；这里只补「非正退出码且拿不到信号
// 信息」的那条——用一个真实的、非 signaled 的退出状态（state.Sys() 是

// ---- follower：跟随 stdout 的读侧逻辑 ----
//
// 它值几行专门的测试：整个「不丢行」的保证都落在这里，且它能脱离子进程独立驱动
// （给一个文件、往里追加、断言交付的行）。

// 每次追加后只交付完整行；未成行的尾部留到 finish（无换行结尾的末行必须交付）。
func TestFollowerDeliversCompleteLinesOnly(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var lines []string
	follow := newFollower(file, func(line []byte) { lines = append(lines, string(line)) })

	// 写半行：不该交付
	if _, err := file.WriteString(`{"partial"`); err != nil {
		t.Fatal(err)
	}
	follow.drain()
	if len(lines) != 0 {
		t.Fatalf("半行不应交付：%v", lines)
	}

	// 补齐这一行并再加一整行
	if _, err := file.WriteString("}\n{\"second\":1}\n"); err != nil {
		t.Fatal(err)
	}
	follow.drain()
	if len(lines) != 2 || lines[0] != `{"partial"}` || lines[1] != `{"second":1}` {
		t.Fatalf("交付 = %v", lines)
	}

	// 追加一行不带换行结尾：drain 不交付，finish 交付
	if _, err := file.WriteString(`{"tail"}`); err != nil {
		t.Fatal(err)
	}
	follow.drain()
	if len(lines) != 2 {
		t.Errorf("未成行的尾部不该被 drain 交付：%v", lines)
	}
	follow.finish()
	if len(lines) != 3 || lines[2] != `{"tail"}` {
		t.Errorf("finish 应交付无换行结尾的末行：%v", lines)
	}
}

// 空行与纯空白行不交付（不产出无意义的进度）。
func TestFollowerSkipsBlankLines(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("a\n\n   \n\t\nb\n"); err != nil {
		t.Fatal(err)
	}
	var lines []string
	follow := newFollower(file, func(line []byte) { lines = append(lines, string(line)) })
	follow.drain()
	if len(lines) != 2 || lines[0] != "a" || lines[1] != "b" {
		t.Errorf("空行应跳过：%v", lines)
	}
}

// 行尾 \r 被去掉（日志不该带上 Windows 换行残留）。
func TestFollowerTrimsCarriageReturn(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("line\r\n"); err != nil {
		t.Fatal(err)
	}
	var lines []string
	follow := newFollower(file, func(line []byte) { lines = append(lines, string(line)) })
	follow.drain()
	if len(lines) != 1 || lines[0] != "line" {
		t.Errorf("应去掉 \\r：%q", lines)
	}
}

// 单行超过一个读取块（followChunk）时不丢内容：跨块的行要能拼接起来。
func TestFollowerJoinsAcrossChunks(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// 写两倍块大小的内容，末尾无换行
	payload := strings.Repeat("x", followChunk*2)
	if _, err := file.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	var lines []string
	follow := newFollower(file, func(line []byte) { lines = append(lines, string(line)) })
	follow.drain()
	follow.finish()
	if len(lines) != 1 {
		t.Fatalf("应拼成一行：%d 行", len(lines))
	}
	if lines[0] != payload {
		t.Errorf("跨块内容被截断：%d 字节，want %d", len(lines[0]), len(payload))
	}
}

// onLine 为 nil 时不 panic。
func TestFollowerNilOnLine(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("a\n"); err != nil {
		t.Fatal(err)
	}
	follow := newFollower(file, nil)
	follow.drain()
	follow.finish()
}

// 进程为 nil 时 run 立刻收尾并交付残余（不阻塞）。
func TestFollowerRunWithNilProcess(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("residual"); err != nil {
		t.Fatal(err)
	}
	var lines []string
	follow := newFollower(file, func(line []byte) { lines = append(lines, string(line)) })
	done := make(chan struct{})
	go func() { defer close(done); follow.run(nil) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("进程为 nil 时 run 不应阻塞")
	}
	if len(lines) != 1 || lines[0] != "residual" {
		t.Errorf("交付 = %v", lines)
	}
}

// 跟随一个真实进程：边跑边交付，退出后残余也交付（实时进度的核心保证）。
func TestFollowerTracksRunningProcess(t *testing.T) {
	requireShell(t)
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	command := exec.Command("sh", "-c", `printf 'first\n'; sleep 0.15; printf 'second\nlast-no-newline'`)
	command.Stdout = file
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	follow := newFollower(file, func(line []byte) {
		mu.Lock()
		lines = append(lines, string(line))
		mu.Unlock()
	})
	done := make(chan struct{})
	go func() { defer close(done); follow.run(command.Process) }()
	_ = command.Wait()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []string{"first", "second", "last-no-newline"}
	if len(lines) != len(want) {
		t.Fatalf("交付 = %v, want %v", lines, want)
	}
	for index, value := range want {
		if lines[index] != value {
			t.Errorf("第 %d 行 = %q, want %q", index, lines[index], value)
		}
	}
}

// drain 的读错误路径：文件不可读时记录下来（交给 Run 上报），而不是静默当读完。
func TestFollowerRecordsReadFailure(t *testing.T) {
	path := t.TempDir() + "/out"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("content\n"); err != nil {
		t.Fatal(err)
	}
	follow := newFollower(file, nil)
	_ = file.Close() // 关掉底层文件：后续 ReadAt 报错

	follow.drain()
	if follow.err() == nil {
		t.Error("读失败应被记录")
	}
	// 已失败后 drain 不再重复尝试（幂等，不 panic）
	follow.drain()
	if follow.err() == nil {
		t.Error("失败状态应保持")
	}
}
