package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

// ctx 取消：进程被终止、Run 返回错误（不静默成功）。
func TestExecRunnerContextCancellation(t *testing.T) {
	requireShell(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := NewExecRunner().Run(ctx, Spec{
		Bin:  "sh",
		Args: []string{"-c", "sleep 30"},
	}, nil)
	if err == nil {
		t.Fatal("ctx 取消后 Run 不应返回 nil")
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

// terminateContainer 的缺省路径真的执行 `docker kill <name>`：用一个假 docker
// 放进 PATH 验证命令行形态（不依赖宿主是否装了 docker）。
func TestTerminateContainerDefaultShellsOutToDocker(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	logFile := dir + "/docker-args"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + logFile + "\n"
	if err := os.WriteFile(dir+"/docker", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	execRunner{}.terminateContainer(Spec{Container: "assistant-review-1"})

	recorded, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("假 docker 未被调用：%v", err)
	}
	// --rm/-i 那种 docker run 前缀不应出现：这里只发 kill <name>
	if got := strings.TrimSpace(string(recorded)); got != "kill\nassistant-review-1" {
		t.Errorf("docker 参数 = %q, want kill + 容器名", got)
	}
}

// 容器名为空不碰 docker（非容器形态不引入一次无谓的 exec）。
func TestTerminateContainerNoopWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	record := dir + "/called"
	script := "#!/bin/sh\ntouch " + record + "\n"
	if err := os.WriteFile(dir+"/docker", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	execRunner{}.terminateContainer(Spec{})
	if _, err := os.Stat(record); err == nil {
		t.Error("容器名为空时不应调用 docker")
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

// stdout 读失败（非 EOF、非「管道已关」）必须作为错误上报，不能静默当成功——
// 否则调用方会把一次读挂掉的会话当正常结束。
func TestExecRunnerPropagatesReadError(t *testing.T) {
	// 子进程被 kill -9 后管道的读端行为由内核决定，不稳定；这里直接驱动 Run 的
	// 那条分支不可行，改断言 isClosedPipe 只认 os.ErrClosed：其他错误一律上报。
	if isClosedPipe(errBoom) {
		t.Error("任意错误不应被当作「管道已关」")
	}
	if !isClosedPipe(os.ErrClosed) {
		t.Error("os.ErrClosed 应被判为「管道已关」")
	}
	if !isClosedPipe(fmt.Errorf("wrapped: %w", os.ErrClosed)) {
		t.Error("包装后的 os.ErrClosed 也应被识别（错误链）")
	}
	if isClosedPipe(nil) {
		t.Error("nil 不应被判为「管道已关」")
	}
}
