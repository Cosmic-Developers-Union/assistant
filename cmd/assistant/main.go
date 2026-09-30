// assistant 是 dev 侧 CLI 入口：交互命令（login / setup / init / install /
// doctor / validate / config / migrate）、CI 自动化（action）与会话 MCP（mcp
// gitea）。命令树与实现都在 internal/cli；常驻进程见 cmd/assistantd。
package main

import (
	"os"

	"github.com/Cosmic-Developers-Union/assistant/internal/cli"
)

func main() {
	os.Exit(cli.Execute(cli.NewRootCommand(os.Stdout, os.Stderr)))
}
