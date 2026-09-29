package sessionstore

import "github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"

// 会话记录的领域类型（键、记录元数据、消息、命中、过滤）定义在 internal/sessionindex
// 里，因为索引才是它们的唯一消费者与查询入口。这里用别名转出，让本包内既有的
// ExtractMessages / Summarize 等抽取逻辑与 MCP 输出层不必感知类型的归属变化——
// 为此曾逐字保留的 extract.go 可以一行不改。
type (
	// Key 见 sessionindex.Key。
	Key = sessionindex.Key
	// Record 见 sessionindex.Record。
	Record = sessionindex.Record
	// Message 见 sessionindex.Message。
	Message = sessionindex.Message
	// Hit 见 sessionindex.Hit。
	Hit = sessionindex.Hit
	// Filter 见 sessionindex.Filter。
	Filter = sessionindex.Filter
)
