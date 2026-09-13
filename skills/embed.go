// Package skills 是随仓库分发的 Agent Skills 源：安装/更新由 skills CLI
// （bunx skills add <repo> --skill review）完成，这里只作为内容的事实来源。
package skills

import _ "embed"

// Review 是 review 技能（skills/review/SKILL.md）的源内容。
//
//go:embed review/SKILL.md
var Review string
