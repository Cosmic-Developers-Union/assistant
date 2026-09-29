package sessionstore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
)

// MCPOptions 是会话记录 MCP 的运行参数。
type MCPOptions struct {
	// Index 是本地检索索引（SQLite）；为空时各工具返回"未配置"的可读错误，
	// 不影响会话其它能力（与 daemon 未启用索引时的行为一致）。
	Index *sessionindex.Store
	// Version 是 assistant 版本（initialize 响应里回给客户端）
	Version string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// RunMCP 以 stdio 提供会话记录查询工具：agent 在上下文被压缩后，用它回查完整历史。
// 数据全部来自本地 SQLite 索引，不碰网络——归档（写）与查询（读）在这里是分开的。
func RunMCP(ctx context.Context, in io.Reader, out io.Writer, options MCPOptions) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	encoder := json.NewEncoder(out)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var request rpcRequest
			if err := json.Unmarshal(line, &request); err == nil {
				response := handleRPC(request, options.Index, options.Version)
				if response != nil {
					if err := encoder.Encode(response); err != nil {
						return err
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func handleRPC(request rpcRequest, index *sessionindex.Store, version string) *rpcResponse {
	reply := func(result any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: request.ID, Result: result}
	}
	fail := func(message string) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: request.ID, Error: &rpcError{Code: -32603, Message: message}}
	}
	switch request.Method {
	case "initialize":
		return reply(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "assistant-sessions", "version": version},
		})
	case "notifications/initialized", "notifications/cancelled":
		return nil
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		return reply(map[string]any{"tools": toolDefinitions()})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return fail("解析 tools/call 参数失败：" + err.Error())
		}
		text, err := callTool(index, params.Name, params.Arguments)
		if err != nil {
			return reply(map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "查询失败：" + err.Error()}},
				"isError": true,
			})
		}
		return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
	}
	return fail("不支持的方法：" + request.Method)
}

func toolDefinitions() []any {
	return []any{
		map[string]any{
			"name": "session_search",
			"description": "在持久化的会话记录里检索（子串、大小写不敏感）。上下文被压缩后用它回查完整聊天/评审历史：" +
				"返回命中的会话键（host/project/session）、会话实体、行号、角色与文本。可用 host/project/conversation/source 收窄。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":        map[string]any{"type": "string", "description": "检索词（子串匹配）"},
					"host":         map[string]any{"type": "string", "description": "限定宿主机标签"},
					"project":      map[string]any{"type": "string", "description": "限定 claude 项目名（评审会话形如 assistant-<host>-<owner>-<repo>）"},
					"conversation": map[string]any{"type": "string", "description": "限定会话实体 id（聊天会话的跨通道标识）"},
					"source":       map[string]any{"type": "string", "enum": []any{"chat", "review", "unknown"}, "description": "限定会话用途"},
					"limit":        map[string]any{"type": "integer", "description": "最多返回多少条命中（缺省 20）"},
				},
				"required": []any{"query"},
			},
		},
		map[string]any{
			"name":        "session_list",
			"description": "列出持久化的会话记录（按更新时间倒序）：键、来源、会话实体、标题、行数、时间与首尾摘要。用来发现有哪些历史会话可查。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":         map[string]any{"type": "string"},
					"project":      map[string]any{"type": "string"},
					"conversation": map[string]any{"type": "string"},
					"source":       map[string]any{"type": "string", "enum": []any{"chat", "review", "unknown"}},
					"limit":        map[string]any{"type": "integer", "description": "缺省 20"},
				},
			},
		},
		map[string]any{
			"name":        "session_read",
			"description": "读取某条会话记录的消息（可分段：offset/limit 按消息计）。用于在 session_search 命中后读取上下文。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":    map[string]any{"type": "string"},
					"project": map[string]any{"type": "string"},
					"session": map[string]any{"type": "string", "description": "会话 id（记录文件名去掉 .jsonl）"},
					"offset":  map[string]any{"type": "integer", "description": "跳过前 N 条消息（缺省 0）"},
					"limit":   map[string]any{"type": "integer", "description": "最多返回多少条消息（缺省 50）"},
				},
				"required": []any{"host", "project", "session"},
			},
		},
		map[string]any{
			"name":        "conversation_list",
			"description": "列出记录库里出现过的会话实体（跨通道的会话 id）及各自的记录条数。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// callTool 执行一次工具调用；index 为空表示索引未启用（工具返回可读错误，
// 而不是让 agent 看到一个空结果里没有数据的假象）。
func callTool(index *sessionindex.Store, name string, raw json.RawMessage) (string, error) {
	if index == nil {
		return "", fmt.Errorf("会话记录索引未启用（检查 config.json 的 sessions 节）")
	}
	arguments := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return "", fmt.Errorf("解析 arguments 失败：%w", err)
		}
	}
	switch name {
	case "session_search":
		query := stringValue(arguments["query"])
		if query == "" {
			return "", fmt.Errorf("query 不能为空")
		}
		hits, err := index.Search(query, Filter{
			Host:         stringValue(arguments["host"]),
			Project:      stringValue(arguments["project"]),
			Conversation: stringValue(arguments["conversation"]),
			Source:       stringValue(arguments["source"]),
		}, intValue(arguments["limit"], 20))
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "没有命中。", nil
		}
		var builder strings.Builder
		for _, hit := range hits {
			fmt.Fprintf(&builder, "%s 行 %d [%s]%s %s\n", hit.Key.String(), hit.Message.Line, hit.Message.Role, conversationLabel(hit.Conversation), snippet(hit.Message.Text, 300))
		}
		return builder.String(), nil
	case "session_list":
		records, err := index.List(Filter{
			Host:         stringValue(arguments["host"]),
			Project:      stringValue(arguments["project"]),
			Conversation: stringValue(arguments["conversation"]),
			Source:       stringValue(arguments["source"]),
		}, intValue(arguments["limit"], 20))
		if err != nil {
			return "", err
		}
		if len(records) == 0 {
			return "记录库里没有会话。", nil
		}
		var builder strings.Builder
		for _, record := range records {
			fmt.Fprintf(&builder, "%s 来源=%s%s 行=%d 更新=%s\n", record.Key.String(), record.Source, conversationLabel(record.Conversation), record.Lines, record.UpdatedAt)
			if record.Title != "" {
				fmt.Fprintf(&builder, "  标题：%s\n", record.Title)
			}
			if record.FirstUserText != "" {
				fmt.Fprintf(&builder, "  开头：%s\n", snippet(record.FirstUserText, 160))
			}
			if record.LastAssistantText != "" {
				fmt.Fprintf(&builder, "  结尾：%s\n", snippet(record.LastAssistantText, 160))
			}
		}
		return builder.String(), nil
	case "session_read":
		key := Key{
			Host:    stringValue(arguments["host"]),
			Project: stringValue(arguments["project"]),
			Session: stringValue(arguments["session"]),
		}
		messages, err := index.Read(key, intValue(arguments["offset"], 0), intValue(arguments["limit"], 50))
		if err != nil {
			return "", err
		}
		if len(messages) == 0 {
			return "该区间没有消息。", nil
		}
		var builder strings.Builder
		for _, message := range messages {
			stamp := ""
			if message.Timestamp != "" {
				if parsed, err := time.Parse(time.RFC3339, message.Timestamp); err == nil {
					stamp = " " + parsed.Local().Format("01-02 15:04")
				}
			}
			fmt.Fprintf(&builder, "行 %d [%s]%s %s\n", message.Line, message.Role, stamp, snippet(message.Text, 1200))
		}
		return builder.String(), nil
	case "conversation_list":
		counts, err := index.Conversations()
		if err != nil {
			return "", err
		}
		if len(counts) == 0 {
			return "没有会话实体。", nil
		}
		ids := make([]string, 0, len(counts))
		for id := range counts {
			ids = append(ids, id)
		}
		sortStrings(ids)
		var builder strings.Builder
		for _, id := range ids {
			fmt.Fprintf(&builder, "%s 记录 %d 条\n", id, counts[id])
		}
		return builder.String(), nil
	}
	return "", fmt.Errorf("未知工具：%s", name)
}

func conversationLabel(conversation string) string {
	if conversation == "" {
		return ""
	}
	return " 会话=" + conversation
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	}
	return fallback
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
