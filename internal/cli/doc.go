// Package cli 承载 assistant 与 assistantd 两个二进制共享的命令层：cobra 命令树、
// 旗标解析、配置与凭据装配，以及统一的信号处理与退出码约定。
//
// 职责边界：本包只负责「把命令行翻译成 internal/* 的调用」，不承载业务语义——
// 调度引擎、评审状态机、对话通道分别在 internal/dispatcher、internal/status、
// internal/daemon。两个入口：
//   - cmd/assistant：dev 侧 CLI（login / setup / init / install / doctor /
//     validate / config / action / mcp），见 NewRootCommand；
//   - cmd/assistantd：常驻进程（调度主循环 + 对话通道同进程），见
//     NewDaemonCommand。
package cli
