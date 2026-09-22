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
var ReviewPrompt = sync.OnceValue(func() string {
	if !strings.HasPrefix(Review, "---\n") {
		return strings.TrimSpace(Review)
	}
	_, rest, _ := strings.Cut(Review, "---\n")
	_, body, found := strings.Cut(rest, "\n---")
	if !found {
		return strings.TrimSpace(Review)
	}
	return strings.TrimSpace(strings.TrimPrefix(body, "\n"))
})
