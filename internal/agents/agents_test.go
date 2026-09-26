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

// SubagentNames 是缺省子代理集的展示口径：不含 main（它不是子代理），且与
// Subagents() 同名同序——两者漂移会让「默认配置」与实际装载的子代理不一致。
func TestSubagentNamesMatchesSubagents(t *testing.T) {
	names := SubagentNames()
	definitions := Subagents()
	if len(names) != len(definitions) {
		t.Fatalf("SubagentNames() = %v, Subagents() 有 %d 项", names, len(definitions))
	}
	for index, definition := range definitions {
		if names[index] != definition.Name {
			t.Errorf("names[%d] = %q, want %q", index, names[index], definition.Name)
		}
	}
	for _, name := range names {
		if name == "main" {
			t.Error("SubagentNames() 不应含 main：它是主代理，不是子代理")
		}
	}
	// 返回的是副本：调用方排序/截断不能改到内置定义
	if len(names) > 1 {
		names[0] = "mutated"
		if SubagentNames()[0] == "mutated" {
			t.Error("SubagentNames() 返回了共享切片")
		}
	}
}
