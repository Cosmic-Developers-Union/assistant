// assistant 是单一二进制入口：instance、mcp、action 与常驻 run 共用命令层。
package main

import (
	"os"

	"github.com/Cosmic-Developers-Union/assistant/internal/cli"
)

func main() {
	os.Exit(cli.Execute(cli.NewRootCommand(os.Stdout, os.Stderr)))
}
