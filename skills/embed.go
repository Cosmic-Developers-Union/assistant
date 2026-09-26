// Package skills 是随仓库分发的 Agent Skills 源：安装/更新由 skills CLI
// （bunx skills add <repo> --skill review）完成，这里只作为内容的事实来源。
package skills

import (
	"strings"
	"sync"

	_ "embed"
)

// Review 是 review 技能（skills/review/SKILL.md）的源内容。
//
//go:embed review/SKILL.md
var Review string

// ReviewPrompt 是 review 技能的正文（剥掉 YAML frontmatter）：内置 review
// agent 系统提示词的唯一事实源——internal/agents 注入预设，调度引擎独立执行，
// 交互技能与本预设定时只改 SKILL.md 一处。
var ReviewPrompt = sync.OnceValue(func() string { return stripFrontmatter(Review) })

// stripFrontmatter 剥掉 YAML frontmatter，返回正文（首尾空白已去）。
//
// 独立成函数而非直接写在 OnceValue 里，是为了让三个分支都能被测到：
// sync.OnceValue 首次求值后就钉死结果、包外无法重置，内联写法下「内容不以
// --- 开头」与「开了 --- 但无闭合定界符」两条兜底分支在任何测试里都不可达，
// 只能靠伪造内嵌内容 + 新进程去凑——那种测试与真实 SKILL.md 脱钩，得不偿失。
func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return strings.TrimSpace(content)
	}
	_, rest, _ := strings.Cut(content, "---\n")
	_, body, found := strings.Cut(rest, "\n---")
	if !found {
		return strings.TrimSpace(content)
	}
	return strings.TrimSpace(strings.TrimPrefix(body, "\n"))
}
