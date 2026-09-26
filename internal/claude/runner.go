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
func (r execRunner) Run(ctx context.Context, spec Spec, onLine func([]byte)) error {
	command := exec.CommandContext(ctx, spec.Bin, spec.Args...)
	command.Dir = spec.Dir
	if len(spec.Env) > 0 {
		command.Env = append(os.Environ(), spec.Env...)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("获取 stdout 管道: %w", err)
	}
	// stderr 落进临时文件而不是内存 writer：**必须是 *os.File**。
	//
	// os/exec 对非 *os.File 的 Stderr 会另建管道并起 goroutine 拷贝，而 Wait() 要
	// 等那份拷贝读到 EOF。子进程把写端交给后代时（`sh -c "sleep 10"` 里的 sleep
	// 就继承了它），EOF 要等后代退出才到——超时终止掉进程后 Wait 仍会挂满整个
	// sleep 时长，超时形同虚设。给 *os.File 时 exec 直接把 fd 交给子进程，Wait
	// 只等进程本身。
	stderrFile, err := os.CreateTemp("", "claude-stderr-*")
	if err != nil {
		return fmt.Errorf("创建 stderr 暂存文件: %w", err)
	}
	defer func() {
		_ = stderrFile.Close()
		_ = os.Remove(stderrFile.Name())
	}()
	command.Stderr = stderrFile
	if err := command.Start(); err != nil {
		return fmt.Errorf("启动 %s: %w", spec.Bin, err)
	}

	var timedOut atomic.Bool
	var timer *time.Timer
	if spec.Timeout > 0 {
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
			time.AfterFunc(KillGrace, func() { _ = kill(command.Process) })
		})
	}

	// stdout 必须在**独立 goroutine** 里读：读管道的调用只在写端全部关闭才返回，
	// 而子进程可能把写端交给了后代（`sh -c "sleep 10"` 里的 sleep 就继承了它）。
	// 若在主 goroutine 里读到 EOF 再 Wait，超时终止掉进程后仍会挂着等那些后代
	// 退出——超时形同虚设。放到 goroutine 后 Wait 能立刻返回，残余的读在管道写端
	// 关闭后自行结束。
	scanDone := make(chan error, 1)
	go func() { scanDone <- scanLines(stdout, onLine) }()
	waitErr := command.Wait()
	if timer != nil {
		timer.Stop()
	}
	scanErr := <-scanDone

	switch {
	case scanErr != nil && !errors.Is(scanErr, io.EOF) && !isClosedPipe(scanErr):
		return fmt.Errorf("读取 %s 输出: %w", spec.Bin, scanErr)
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

// terminateContainer 在容器形态下按名终止容器；失败不影响其余收尾（容器可能
// 已自行退出）。
func (r execRunner) terminateContainer(spec Spec) {
	if spec.Container == "" {
		return
	}
	kill := r.killContainer
	if kill == nil {
		kill = func(name string) error {
			return exec.Command("docker", "kill", name).Run()
		}
	}
	_ = kill(spec.Container)
}

// isClosedPipe 报告错误是否只是「管道已被 Wait 关掉」（os.ErrClosed）。
//
// 进程死亡后 Wait 会关闭自己那侧的管道，此时正在读的 goroutine 会拿到
// os.ErrClosed 而不是 io.EOF。那不是读取失败——我们本来就要在这一刻收手——所以
// 不能让它掩盖真正的原因（退出码、超时）。
func isClosedPipe(err error) bool {
	return errors.Is(err, os.ErrClosed)
}

// scanLines 逐行交付 stdout；返回读取错误（含 io.EOF）。
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
