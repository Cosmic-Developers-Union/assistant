// Package agents 是内嵌（随二进制分发）的命名 agent 预设：system prompt 与
// MCP server 定义都打包在二进制里，聊天命令（/agent）与配置（default_agent、
// 通道 agent）按名直接引用；用户 config.json 的同名 agent 覆盖内置同名预设。
package agents

import (
	_ "embed"
	jsonv2 "encoding/json/v2"
	"slices"
	"sync"
)

// Definition 是一个内置 agent 预设。
type Definition struct {
	// Name 是预设名（/agent <名>、default_agent、通道 agent 引用它）
	Name string `json:"name"`
	// Description 是一句话说明（/agent 列表与文档展示）
	Description string `json:"description"`
	// SystemPrompt 非空时整体替换对话会话的基础系统提示词
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MCP 是随预设下发的 MCP server 定义（.mcp.json 形态）：合并进会话 MCP
	// 配置，同名 server 覆盖自举（daemon/sessions）与供应商的
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
	return document.Agents
})

// List 返回全部内置 agent（编译期内嵌，顺序稳定）。
func List() []Definition { return builtin() }

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

// Names 返回内置 agent 名（字典序）。
func Names() []string {
	names := make([]string, 0, len(builtin()))
	for _, definition := range builtin() {
		names = append(names, definition.Name)
	}
	slices.Sort(names)
	return names
}
