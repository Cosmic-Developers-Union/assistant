package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

// exitCodeAuthFailure 遵循 sysexits(3) 的 EX_CONFIG：配置类致命错误（认证被拒、
// Gitea 地址不正确）。CI 与本地调用方据此区分配置错误与瞬时错误。
const exitCodeAuthFailure = 78

// Execute 运行命令并返回进程退出码。首个信号取消 ctx，各命令优雅收尾（run 处理
// 完当前待办）；再次信号强杀。assistant 的唯一入口经由这里，保证所有命令的信号语义与退出码约定一致。
func Execute(command *cobra.Command) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	var interrupted atomic.Int32
	go func() {
		count := 0
		for {
			var sig os.Signal
			select {
			case <-done:
				return
			case sig = <-signals:
			}
			count++
			code := 143
			if sig == os.Interrupt {
				code = 130
			}
			interrupted.Store(int32(code))
			if count == 1 {
				fmt.Fprintf(os.Stderr, "收到 %s，正在退出（再次发送将立即强制退出）\n", sig)
				cancel()
				continue
			}
			fmt.Fprintf(os.Stderr, "再次收到 %s，强杀退出\n", sig)
			os.Exit(code)
		}
	}()

	if err := command.ExecuteContext(ctx); err != nil {
		if code := interrupted.Load(); code != 0 {
			return int(code)
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "已取消")
			return 130
		}
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		if status.IsFatalError(err) {
			return exitCodeAuthFailure
		}
		return 1
	}
	if code := interrupted.Load(); code != 0 {
		return int(code)
	}
	return 0
}
