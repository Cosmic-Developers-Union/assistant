package skills

import (
	"strings"
	"testing"
)

// 真实内嵌内容的基本形状：非空、正文不含 frontmatter 定界符、正文非空。
func TestReviewEmbedded(t *testing.T) {
	if strings.TrimSpace(Review) == "" {
		t.Fatal("内嵌的 review/SKILL.md 不应为空")
	}
	if !strings.HasPrefix(Review, "---\n") {
		t.Error("SKILL.md 应以 YAML frontmatter 定界符 --- 开头（约定形态）")
	}
	if !strings.Contains(Review, "name: review") {
		t.Error("frontmatter 应声明 name: review")
	}
	if !strings.Contains(Review, "\n---") {
		t.Error("frontmatter 应有闭合定界符")
	}
}

// ReviewPrompt 必须真的剥掉 frontmatter：正文里不得再出现 name:/description:
// 头部字段，且确实保留了 SKILL.md 中的正文内容。
func TestReviewPromptStripsFrontmatter(t *testing.T) {
	prompt := ReviewPrompt()

	if strings.TrimSpace(prompt) == "" {
		t.Fatal("ReviewPrompt() 不应为空")
	}
	if strings.HasPrefix(prompt, "---") {
		t.Error("ReviewPrompt() 不应以 frontmatter 定界符开头")
	}
	if strings.Contains(prompt, "name: review") {
		t.Error("ReviewPrompt() 应已剥离 frontmatter 的 name 字段")
	}
	if strings.Contains(prompt, "description:") {
		t.Error("ReviewPrompt() 应已剥离 frontmatter 的 description 字段")
	}
	if prompt != strings.TrimSpace(prompt) {
		t.Error("ReviewPrompt() 应去掉首尾空白")
	}
	if !strings.HasPrefix(prompt, "# review") {
		t.Errorf("ReviewPrompt() 应从正文标题开始，实际开头：%q", firstLine(prompt))
	}
	if !strings.Contains(prompt, "PR 审查协议") {
		t.Error("ReviewPrompt() 应保留 SKILL.md 的正文内容")
	}

	// 剥离后的正文必须真的来自 Review（而不是凭空拼出来的）。
	if !strings.Contains(Review, prompt) {
		t.Error("ReviewPrompt() 的正文应为 Review 的子串（纯剥离，无改写）")
	}
}

// 同一个进程内多次调用必须返回同一份记忆化结果。
func TestReviewPromptMemoized(t *testing.T) {
	first := ReviewPrompt()
	second := ReviewPrompt()
	if first != second {
		t.Error("ReviewPrompt 是 sync.OnceValue，多次调用应返回同一结果")
	}
}

// stripFrontmatter 的三个分支：正常剥离、不以定界符开头、开了定界符但没闭合。
//
// 测的是剥签逻辑本身而非记忆化的 ReviewPrompt：后者是 sync.OnceValue，首次求值
// 即钉死结果、包外无法重置，内联写法下后两条兜底分支在任何单进程测试里都不可达。
func TestStripFrontmatter(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "正常剥离 frontmatter",
			content: "---\nname: review\ndescription: x\n---\n# 标题\n正文",
			want:    "# 标题\n正文",
		},
		{
			name:    "不以定界符开头：原样返回并去空白",
			content: "\n\n# 只有正文\n",
			want:    "# 只有正文",
		},
		{
			name:    "有 --- 开头但无闭合定界符：退回整篇去空白",
			content: "---\nname: review\n# 忘了闭合",
			want:    "---\nname: review\n# 忘了闭合",
		},
		{
			name:    "frontmatter 后正文首部的换行被去掉",
			content: "---\nname: review\n---\n\n\n# 标题",
			want:    "# 标题",
		},
		{
			name:    "空内容",
			content: "   \n\t",
			want:    "",
		},
		{
			name:    "只有 frontmatter 没有正文",
			content: "---\nname: review\n---\n",
			want:    "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := stripFrontmatter(test.content); got != test.want {
				t.Errorf("stripFrontmatter(%q) = %q, want %q", test.content, got, test.want)
			}
		})
	}
}

// 真实内嵌内容的剥离结果与 stripFrontmatter 的直接调用一致：证明 ReviewPrompt
// 确实只是「取内嵌内容 + 剥 frontmatter」，没有额外改写。
func TestReviewPromptMatchesStripFrontmatter(t *testing.T) {
	if got, want := ReviewPrompt(), stripFrontmatter(Review); got != want {
		t.Errorf("ReviewPrompt() 与 stripFrontmatter(Review) 不一致：%q vs %q", got, want)
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
