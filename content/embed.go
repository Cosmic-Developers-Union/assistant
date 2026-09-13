// Package content 是 assistant install 写入仓库的内容源，由维护者编写：
// 修改这里的文件即可更新生成内容，无需改代码。
package content

import _ "embed"

// AgentsSection 是 AGENTS.md 的 assistant 段落（含 marker，可含 <<.Reviewer>>
// <<.Merger>> 等绑定占位符）。
//
//go:embed agents.md
var AgentsSection string
