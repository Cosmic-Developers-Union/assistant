package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"assistant/internal/status"
)

var version = "dev"

// exitCodeAuthFailure 遵循 sysexits(3) 的 EX_CONFIG：配置类致命错误（认证被拒、
// Gitea 地址不正确）。CI 与本地调用方据此区分配置错误与瞬时错误。
const exitCodeAuthFailure = 78

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 首个信号：取消 ctx，各命令优雅收尾（run 处理完当前待办）；再次信号：强杀。
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		count := 0
		for sig := range signals {
			count++
			if count == 1 {
				fmt.Fprintf(os.Stderr, "收到 %s，等待当前操作完成后退出（再次发送将立即强杀）\n", sig)
				cancel()
				continue
			}
			fmt.Fprintf(os.Stderr, "再次收到 %s，强杀退出\n", sig)
			os.Exit(1)
		}
	}()

	command := newRootCommand(os.Stdout, os.Stderr, runCheck, runLabelSync, runAutoMerge)
	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		if status.IsFatalError(err) {
			os.Exit(exitCodeAuthFailure)
		}
		os.Exit(1)
	}
}
