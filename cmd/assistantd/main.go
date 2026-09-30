// assistantd 是常驻进程入口：调度主循环（检测待办 → 每待办一个会话 → 验证 →
// 清理）与对话通道在同一进程内长驻。旗标面与 `assistant run` 一致，命令实现
// 在 internal/cli；会话里的 gitea MCP 由同机的 assistant 二进制提供——
// assistantd 依赖 assistant，反过来 assistant 不依赖 assistantd。
package main

import (
	"os"

	"github.com/Cosmic-Developers-Union/assistant/internal/cli"
)

func main() {
	os.Exit(cli.Execute(cli.NewDaemonCommand()))
}
