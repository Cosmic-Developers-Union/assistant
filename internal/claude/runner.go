// Package claude 封装与 claude CLI 的全部交互：进程启动、参数与环境组装、
// stream-json 输出解析。
//
// 为什么单独成包：CLI 是本项目唯一的模型运行时依赖，此前 dispatcher（评审会话）
// 与 daemon（对话会话）各自集成了一遍——两套 argv 组装、两套 stream-json 模型与
// 解析器、两套结果结构体。CLI 改一个字段名就要改两处，且真正的 exec 埋在业务函数
// 里无法独立测试（只能靠往磁盘上写假可执行脚本驱动）。
//
// 本包的分层（上层只依赖可替换的部分，真实进程只剩最薄一层）：
//   - Runner 接口：唯一需要注入的执行点。业务代码依赖它；生产用 NewExecRunner()，
//     测试用假实现喂预置的 stream-json 行或注入 spawn/超时失败。
//   - BuildArgs / SessionEnv：纯函数，逐 flag、逐环境变量可断言，不碰进程与文件系统。
//   - ParseLine / Feed：一份 stream-json 模型与解析器（此前是两份）。
//   - Outcome：一次会话的归集结果（此前是 SessionOutcome 与 chatOutcome 两个近亲）。
//
// 边界：本包只管「怎么跟 CLI 说话」，不管「谁该被评审」「会话何时算完成」——那些是
// dispatcher/status 的语义。
package claude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// 执行策略常量（与 CLI 的交互契约，不是可调参数）。
const (
	// StderrTailChars 是失败诊断保留的 stderr 尾部长度：宁可少留也不刷屏，
	// 完整输出在会话文本记录里。
	StderrTailChars = 2_000
	// ScanBufferMax 是单行上限：stream-json 的单条消息可能很长（大 diff、
	// base64），默认的 bufio.Scanner 上限（64KiB）会截断。
	ScanBufferMax = 8 * 1024 * 1024
	// ScanBufferInitial 是扫描缓冲的初始容量。
	ScanBufferInitial = 64 * 1024
)

// KillGrace 是超时 SIGTERM 后的优雅退出窗口，超此改 SIGKILL。
//
// 是 var 而非 const：这是唯一需要「等一段时间」的路径，测试要能把它调短来验证
// 升级到 SIGKILL（同 ThinkingProgressInterval 的理由）。生产不修改它。
var KillGrace = 5 * time.Second

// Spec 是一次 CLI 调用的完整输入：纯数据，可直接写进测试期望。
type Spec struct {
	// Bin 是可执行文件（容器形态下为 "docker"）。
	Bin string
	// Args 是已组装好的 argv（含容器形态的 run --rm … 前缀）。
	Args []string
	// Dir 是子进程的工作目录。
	Dir string
	// Env 是追加到 os.Environ() 之外的进程环境变量（KEY=VALUE）。为空时子进程
	// 继承父环境。
	Env []string
	// Timeout 是会话超时；0 表示不限时（由 ctx 控制）。
	Timeout time.Duration
	// Container 非空时表示本次是容器形态：超时后额外 `docker kill <name>`——
	// 终止 docker 客户端不会停掉容器，必须按名兜底。
	Container string
}

// Runner 执行一次 CLI 调用，并把 stdout 逐行交给 onLine。
//
// 返回值的语义与子进程退出状态一致：
//   - nil：进程正常退出（退出码 0）；
//   - 非 nil：无法启动、非零退出、被信号终止或超时——错误信息已尽量带上可读的
//     原因（退出码/信号名/stderr 尾部）。
//
// 实现须在返回前交付完 stdout 的全部内容（包括无换行结尾的残余行），否则调用方
// 会丢掉最后一条 stream-json 消息。
type Runner interface {
	Run(ctx context.Context, spec Spec, onLine func([]byte)) error
}

// execRunner 是 Runner 的生产实现，也是本包唯一启动子进程的地方。
type execRunner struct {
	// terminate 是超时后的第一步终止动作，默认发 SIGTERM；测试可替换为假动作
	// （真实信号在单测里没有可观察的副作用，注入后能断言「超时确实触发过」）。
	terminate func(process *os.Process) error
	// kill 是宽限期后的兜底终止，默认 SIGKILL。
	kill func(process *os.Process) error
	// killContainer 终止容器，默认执行 `docker kill`。
	killContainer func(name string) error
}

// NewExecRunner 返回启动真实子进程的 Runner。
func NewExecRunner() Runner { return execRunner{} }

// Run 实现 Runner：启动子进程、逐行消费 stdout、按退出状态归类错误。
//
// stdout 与 stderr **都落进真实临时文件**，再由一个跟随 goroutine 边写边读
// stdout 交付给 onLine。为什么不直接用管道（StdoutPipe / io.Writer）：
//
//   - command.Stdout 给 io.Writer 时，os/exec 会另起 goroutine 拷贝，而 Wait()
//     要等那份拷贝读到 EOF；
//   - StdoutPipe 的读端由 Wait() 关闭，机器有负载时 Wait 会赶在 reader 取完内核
//     缓冲之前关掉它，于是丢行——最后一条 result 消息正是判会话结论的依据。
//
// 两处都栽在同一件事上：子进程常把写端交给后代（`sh -c "sleep 10"` 里的 sleep
// 就继承了它），于是「等 EOF」等的是后代退出，超时终止掉直接子进程也解不开——
// 超时形同虚设。给 *os.File 时 exec 直接把 fd 交给子进程、不起拷贝 goroutine，
// Wait 只等进程本身；文件内容一直留到我们读完为止，一行都不会丢。
func (r execRunner) Run(ctx context.Context, spec Spec, onLine func([]byte)) error {
	command := exec.CommandContext(ctx, spec.Bin, spec.Args...)
	command.Dir = spec.Dir
	if len(spec.Env) > 0 {
		command.Env = append(os.Environ(), spec.Env...)
	}

	stdoutFile, err := os.CreateTemp("", "claude-stdout-*")
	if err != nil {
		return fmt.Errorf("创建 stdout 暂存文件: %w", err)
	}
	defer func() {
		_ = stdoutFile.Close()
		_ = os.Remove(stdoutFile.Name())
	}()
	stderrFile, err := os.CreateTemp("", "claude-stderr-*")
	if err != nil {
		return fmt.Errorf("创建 stderr 暂存文件: %w", err)
	}
	defer func() {
		_ = stderrFile.Close()
		_ = os.Remove(stderrFile.Name())
	}()
	command.Stdout = stdoutFile
	command.Stderr = stderrFile

	if err := command.Start(); err != nil {
		return fmt.Errorf("启动 %s: %w", spec.Bin, err)
	}

	var timedOut atomic.Bool
	var timer *time.Timer
	if spec.Timeout > 0 {
		// 在进超时闭包前读一次 KillGrace：闭包跑在自己的 goroutine 上，直接读包级
		// 变量会与「测试替换它」形成数据竞争（-race 可复现）。
		grace := KillGrace
		timer = time.AfterFunc(spec.Timeout, func() {
			timedOut.Store(true)
			r.terminateContainer(spec)
			terminate := r.terminate
			if terminate == nil {
				terminate = terminateProcess
			}
			_ = terminate(command.Process)
			kill := r.kill
			if kill == nil {
				kill = func(process *os.Process) error { return process.Kill() }
			}
			time.AfterFunc(grace, func() { _ = kill(command.Process) })
		})
	}

	// 跟随 stdout：进程运行期间持续交付新增行（保持实时进度），进程退出后收尾。
	// 与 Wait 互不阻塞——两边读的是同一个文件，谁先谁后都不丢数据。
	follow := newFollower(stdoutFile, onLine)
	followDone := make(chan struct{})
	go func() {
		defer close(followDone)
		follow.run(command.Process)
	}()

	waitErr := command.Wait()
	if timer != nil {
		timer.Stop()
	}
	<-followDone
	readErr := follow.err()

	switch {
	case readErr != nil:
		return fmt.Errorf("读取 %s 输出: %w", spec.Bin, readErr)
	case waitErr != nil && command.ProcessState == nil:
		// 进程未正常跑起来（如 ctx 取消打断 Start→Wait 之间）
		return waitErr
	case timedOut.Load():
		return fmt.Errorf("会话超时（>%dms），已 SIGTERM 终止", spec.Timeout.Milliseconds())
	}

	if exitCode := command.ProcessState.ExitCode(); exitCode != 0 {
		how := describeExit(command.ProcessState, exitCode)
		if tail := strings.TrimSpace(readTail(stderrFile, StderrTailChars)); tail != "" {
			return fmt.Errorf("claude %s：%s", how, tail)
		}
		return fmt.Errorf("claude %s", how)
	}
	return nil
}

// followInterval 是跟随 stdout 的轮询间隔。文件读写没有事件通知，只能轮询；
// 20ms 兼顾实时感（进度行看起来是「正在发生」）与代价（一轮会话几十分钟，
// 每秒 50 次很小的 ReadAt）。
const followInterval = 20 * time.Millisecond

// followChunk 是单次读取的块大小。
const followChunk = 64 * 1024

// follower 跟随一个正被写入的文件，把新增内容按行交付。
//
// 为什么不用管道：见 Run 的注释——管道要么让 Wait 阻塞在后代持有的写端上，要么
// 被 Wait 关掉读端而丢行。文件没有这两个问题：进程死后内容仍在，可以从容读完；
// 跟随只是为了让进度保持实时。
type follower struct {
	file    *os.File
	onLine  func([]byte)
	offset  int64
	partial []byte
	failure error
}

func newFollower(file *os.File, onLine func([]byte)) *follower {
	return &follower{file: file, onLine: onLine}
}

// run 循环交付新增内容，直到进程退出；退出后再读一轮把残余（含无换行结尾的
// 末行）交付完。
func (f *follower) run(process *os.Process) {
	for {
		f.drain()
		if process != nil && processExited(process) {
			f.drain()
			f.finish()
			return
		}
		if process == nil {
			f.finish()
			return
		}
		time.Sleep(followInterval)
	}
}

// drain 读走自上次以来新增的全部内容，按行交付；未成行的尾部留待 finish。
func (f *follower) drain() {
	if f.failure != nil {
		return
	}
	buffer := make([]byte, followChunk)
	for {
		count, err := f.file.ReadAt(buffer, f.offset)
		if count > 0 {
			f.offset += int64(count)
			f.consume(buffer[:count])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				f.failure = err
			}
			return
		}
		if count < len(buffer) {
			return
		}
	}
}

// consume 按换行切分新内容：完整行立即交付，剩余部分留作下次。
func (f *follower) consume(chunk []byte) {
	f.partial = append(f.partial, chunk...)
	for {
		index := bytes.IndexByte(f.partial, '\n')
		if index < 0 {
			return
		}
		f.emit(f.partial[:index])
		f.partial = f.partial[index+1:]
	}
}

// finish 交付没有换行结尾的末行：CLI 的 result 消息可能不带换行，丢了它调用方
// 就看不到会话结论。
func (f *follower) finish() {
	if len(f.partial) > 0 {
		f.emit(f.partial)
		f.partial = nil
	}
}

// emit 交付一行；空行与纯空白行跳过（不产出无意义的进度）。
func (f *follower) emit(line []byte) {
	trimmed := bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(trimmed)) == 0 {
		return
	}
	if f.onLine != nil {
		f.onLine(trimmed)
	}
}

func (f *follower) err() error { return f.failure }

// readTail 读文件尾部至多 limit 字节（stderr 的尾部才是诊断要点，整段会把日志
// 淹掉；完整输出在会话文本记录里）。
func readTail(file *os.File, limit int) string {
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	offset := int64(0)
	if size > int64(limit) {
		offset = size - int64(limit)
	}
	buffer := make([]byte, size-offset)
	if _, err := file.ReadAt(buffer, offset); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return string(buffer)
}

// dockerKillCommand 执行 `docker kill <name>`；单独抽成 var 是为了让
// terminateContainer 的缺省路径可在测试里断言命令行（否则要么真去调 docker、
// 要么改 PATH——后者是进程级状态，会干扰并发跑的其它测试）。生产不修改。
var dockerKillCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// terminateContainer 在容器形态下按名终止容器；失败不影响其余收尾（容器可能
// 已自行退出）。
func (r execRunner) terminateContainer(spec Spec) {
	if spec.Container == "" {
		return
	}
	kill := r.killContainer
	if kill == nil {
		kill = func(name string) error {
			return dockerKillCommand("docker", "kill", name)
		}
	}
	_ = kill(spec.Container)
}

// scanLines 逐行交付一个 reader；返回读取错误（含 io.EOF）。
//
// 末行没有换行符时也要交付：CLI 的 result 消息是最后一行，且在流被中断时可能
// 不带换行——丢了它调用方就看不到会话结论。
func scanLines(reader io.Reader, onLine func([]byte)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, ScanBufferInitial), ScanBufferMax)
	for scanner.Scan() {
		if onLine != nil {
			onLine(scanner.Bytes())
		}
	}
	return scanner.Err()
}

// describeExit 把退出状态折成可读原因：正退出码给数字，被信号终止给信号名，
// 其余（如 core dump）说明异常终止。
func describeExit(state *os.ProcessState, exitCode int) string {
	switch {
	case exitCode > 0:
		return fmt.Sprintf("退出码 %d", exitCode)
	default:
		if signaled, name := ProcessSignaled(state); signaled {
			return fmt.Sprintf("被信号 %s 终止", name)
		}
		return "异常终止"
	}
}
