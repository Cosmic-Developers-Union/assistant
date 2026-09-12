package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"assistant/internal/status"
)

const version = "dev"

// exitCodeAuthFailure 遵循 sysexits(3) 的 EX_CONFIG：配置类致命错误（认证被拒、
// Gitea 地址不正确）。CI 与本地调用方据此区分配置错误与瞬时错误。
const exitCodeAuthFailure = 78

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := newRootCommand(os.Stdout, os.Stderr, runCheck, runSync, runAutoMerge)
	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		if status.IsFatalError(err) {
			os.Exit(exitCodeAuthFailure)
		}
		os.Exit(1)
	}
}
