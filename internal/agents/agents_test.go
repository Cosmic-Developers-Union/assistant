package agents

import (
	"slices"
	"strings"
	"testing"
)

// 内嵌预设随二进制分发：一个内置主 agent + 若干子代理、名字唯一、提示词非空、
// 可按名查找。
func TestBuiltinAgents(t *testing.T) {
	names := Names()
	if !slices.Equal(names, []string{"coder", "main", "ops", "review", "writer"}) {
		t.Errorf("Names = %v", names)
	}
	for _, definition := range List() {
		if strings.TrimSpace(definition.Name) == "" || strings.TrimSpace(definition.Description) == "" {
			t.Errorf("内置 agent 缺名字或描述：%+v", definition)
		}
		if strings.TrimSpace(definition.SystemPrompt) == "" {
			t.Errorf("内置 agent %s 缺系统提示词", definition.Name)
		}
	}
	main := Main()
	if main.Name != MainAgentName || !strings.Contains(main.SystemPrompt, "主 agent") {
		t.Errorf("Main() = %+v", main)
	}
	subagents := Subagents()
	got := make([]string, 0, len(subagents))
	for _, definition := range subagents {
		got = append(got, definition.Name)
	}
	if !slices.Equal(got, []string{"coder", "ops", "review", "writer"}) {
		t.Errorf("Subagents = %v", got)
	}
	review, ok := Lookup("review")
	if !ok || !strings.Contains(review.SystemPrompt, "PR 审查协议") {
		t.Error("review 提示词应注入 skills/review/SKILL.md 正文")
	}
	if definition, ok := Lookup("ops"); !ok || definition.Name != "ops" {
		t.Errorf("Lookup(ops) = %+v ok=%v", definition, ok)
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup(nope) 不应命中")
	}
	if !Has("writer") || Has("") {
		t.Error("Has 判定不对")
	}
}
