package daemon

import (
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
)

// chatOutcome 是对话会话一轮的归集结果。
//
// 与评审会话不同，对话会话只需要「最终回复文本 + 出错原因（尤其是 API 报错）」。
// 保留本包自己的类型（而非直接用 claude.Outcome）是因为下游（FailureMessage、
// 元数据写入、日志）按这里的字段读写；解析逻辑则统一在 internal/claude。
type chatOutcome struct {
	Result    string
	SessionID string
	Model     string
	IsError   bool
	Subtype   string
	CostUSD   float64
	NumTurns  int
	Errors    []string
	// ThinkingTokens 是本轮模型思考的估算 token 数（thinking_tokens 事件）
	ThinkingTokens int
	// MCPStatus 是 init 事件里各 MCP server 的连通状态（name → status）
	MCPStatus map[string]string
	// APIError 是 claude 上报的 API 层错误（如 401 invalid api key）：这是排查
	// 「发消息没反应」最关键的一行，必须能透到日志与回复里。
	APIError string
}

// chatOutcomeOf 把统一的归集结果转成本包的视图。
//
// 字段一一对应；thinking 的播报节流水位不进归集结果（它是解析器的内部状态）。
func chatOutcomeOf(outcome claude.Outcome) chatOutcome {
	return chatOutcome{
		Result:         outcome.Result,
		SessionID:      outcome.SessionID,
		Model:          outcome.Model,
		IsError:        outcome.IsError,
		Subtype:        outcome.Subtype,
		CostUSD:        outcome.CostUSD,
		NumTurns:       outcome.NumTurns,
		Errors:         outcome.Errors,
		ThinkingTokens: outcome.ThinkingTokens,
		MCPStatus:      outcome.MCPStatus,
		APIError:       outcome.APIError,
	}
}

// chatStreamOutcome 把本包的视图灌回统一结构（chatOutcomeOf 的逆向），供逐行解析
// 复用：Feed 是无状态函数，「上一行已归集的部分」必须整份带上，否则每行都从零开始，
// 会话 id / API 错误 / result 字段都会被下一行抹掉。
func chatStreamOutcome(outcome chatOutcome) claude.Outcome {
	return claude.Outcome{
		Result:         outcome.Result,
		SessionID:      outcome.SessionID,
		Model:          outcome.Model,
		IsError:        outcome.IsError,
		Subtype:        outcome.Subtype,
		CostUSD:        outcome.CostUSD,
		NumTurns:       outcome.NumTurns,
		Errors:         outcome.Errors,
		ThinkingTokens: outcome.ThinkingTokens,
		MCPStatus:      outcome.MCPStatus,
		APIError:       outcome.APIError,
	}
}

// feedChatStreamLine 解析一行 stream-json 并累积进 outcome，返回它是否为 result。
//
// 解析与进度呈现统一在 internal/claude（此前 dispatcher 与 daemon 各有一份近亲
// 实现，CLI 改一个字段名要改两处，行为也已出现细微差异）。对话会话用展开形态：
// init 实况逐字段、工具入参按缩进 JSON 多行——交互式排查时这些细节都要看得见。
func feedChatStreamLine(outcome *chatOutcome, line []byte, onProgress func(string)) bool {
	// 整份灌进统一结构（含 MCP 状态：Feed 依赖它判未就绪的 server），解析完再整体
	// 取回——少带一个字段就等于该字段只活一行。
	unified := chatStreamOutcome(*outcome)
	done := claude.Feed(&unified, line, claude.ProgressVerbose, onProgress)
	*outcome = chatOutcomeOf(unified)
	return done
}

// FailureMessage 汇总失败原因：优先 API 层错误（401/超时等），其次是 result 的
// errors/subtype。这样「没反应」的回复能直接说清是认证失败还是模型拒绝。
func (o chatOutcome) FailureMessage() string {
	if message := strings.TrimSpace(o.APIError); message != "" {
		return "模型端点报错：" + message
	}
	if len(o.Errors) > 0 {
		return strings.Join(o.Errors, "；")
	}
	if o.Subtype != "" {
		return "会话执行失败（" + o.Subtype + "）"
	}
	return "会话执行失败"
}
