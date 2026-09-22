package agents

import (
	"slices"
	"strings"
	"testing"
)

// 内嵌预设随二进制分发：三个内置 agent、名字唯一、提示词非空、可按名查找。
func TestBuiltinAgents(t *testing.T) {
	names := Names()
	if !slices.Equal(names, []string{"coder", "ops", "writer"}) {
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
