// Package agents 是内嵌（随二进制分发）的命名 agent 预设：main 是内置通用
// 主 agent（runtime 未指定 main_agent 时的缺省接待者），其余是子代理预设
// （注入会话供主模型委派；review 还被调度引擎独立执行）。用户 config.json
// 的同名 agent 覆盖内置同名预设。
package agents

import (
	_ "embed"
	jsonv2 "encoding/json/v2"
	"slices"
	"strings"
	"sync"

	"assistant/skills"
)

// MainAgentName 是内置主 agent 的名字（runtime.main_agent 的缺省值）。
const MainAgentName = "main"

// Definition 是一个内置 agent 预设。
type Definition struct {
	// Name 是预设名（runtime.main_agent / subagents 按名引用）
	Name string `json:"name"`
	// Description 是一句话说明（子代理委派时主模型看它决定何时派给谁）
	Description string `json:"description"`
	// SystemPrompt 是该 agent 的系统提示词
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MCP 是随预设下发的 MCP server 定义（.mcp.json 形态）：子代理的并入
	// 所在会话 MCP 配置，同名 server 覆盖自举（daemon/sessions）与供应商的
	MCP map[string]any `json:"mcp,omitempty"`
	// Model 可选模型覆盖（留空用账号默认，可被用户同名覆盖改掉）
	Model string `json:"model,omitempty"`
}

//go:embed builtin.json
var builtinJSON []byte

var builtin = sync.OnceValue(func() []Definition {
	var document struct {
		Agents []Definition `json:"agents"`
	}
	// 内嵌文件随二进制编译，坏档是编译期事故：直接 panic 让它显式炸出来
	if err := jsonv2.Unmarshal(builtinJSON, &document); err != nil {
		panic("internal/agents: builtin.json 损坏: " + err.Error())
	}
	for index, definition := range document.Agents {
		// review 的提示词事实源是 skills/review/SKILL.md（交互技能与本预设
		// 共用一份文本），这里在加载期注入
		if definition.Name == "review" && strings.TrimSpace(definition.SystemPrompt) == "" {
			document.Agents[index].SystemPrompt = skills.ReviewPrompt()
		}
	}
	return document.Agents
})

// List 返回全部内置 agent（编译期内嵌，顺序稳定）。
func List() []Definition { return builtin() }

// Main 返回内置主 agent 预设。
func Main() Definition {
	definition, ok := Lookup(MainAgentName)
	if !ok {
		panic("internal/agents: builtin.json 缺少 main 主 agent 预设")
	}
	return definition
}

// Subagents 返回全部内置子代理（除 main 外，名字字典序）。
func Subagents() []Definition {
	definitions := make([]Definition, 0, len(builtin()))
	for _, definition := range builtin() {
		if definition.Name != MainAgentName {
			definitions = append(definitions, definition)
		}
	}
	slices.SortFunc(definitions, func(a, b Definition) int { return strings.Compare(a.Name, b.Name) })
	return definitions
}

// Lookup 按名取内置 agent。
func Lookup(name string) (Definition, bool) {
	for _, definition := range builtin() {
		if definition.Name == name {
			return definition, true
		}
	}
	return Definition{}, false
}

// Has 报告名字是否是内置 agent。
func Has(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// Names 返回内置 agent 名（字典序，含 main）。
func Names() []string {
	names := make([]string, 0, len(builtin()))
	for _, definition := range builtin() {
		names = append(names, definition.Name)
	}
	slices.Sort(names)
	return names
}

// SubagentNames 返回内置子代理名（字典序，不含 main；缺省子代理集的展示用）。
func SubagentNames() []string {
	definitions := Subagents()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
