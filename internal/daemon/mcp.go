package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// mcpProtocolVersion 是实现的 MCP 协议版本（stdio JSON-RPC，按行分隔）。
const mcpProtocolVersion = "2024-11-05"

// MCP 工具名。
const (
	ToolStatus        = "daemon_status"
	ToolListSessions  = "list_sessions"
	ToolListQueue     = "list_queue"
	ToolRecentResults = "recent_results"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// RunMCP 提供自举的 daemon 状态 MCP（stdio）：自己发现运行中的 daemon
// （端点文件 / ASSISTANT_DAEMON_ENDPOINT），工具全部只读。
func RunMCP(ctx context.Context, in io.Reader, out io.Writer, getenv func(string) string, version string) error {
	reader := bufio.NewScanner(in)
	reader.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	writer := bufio.NewWriter(out)
	defer writer.Flush()

	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}
		var request rpcRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			// 无法解析的行直接忽略（stdio 上混入杂音不致命）
			continue
		}
		if len(request.ID) == 0 || string(request.ID) == "null" {
			continue // notification：不需要响应
		}
		response := handleRPC(ctx, request, getenv, version)
		encoded, err := json.Marshal(response)
		if err != nil {
			continue
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			return err
		}
		_ = writer.Flush()
	}
	return reader.Err()
}

func handleRPC(ctx context.Context, request rpcRequest, getenv func(string) string, version string) rpcResponse {
	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{"name": "assistant-daemon", "version": version},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": mcpToolDefinitions()}
	case "tools/call":
		result, err := callTool(ctx, request.Params, getenv)
		if err != nil {
			response.Result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return response
		}
		response.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": result}},
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found: " + request.Method}
	}
	return response
}

func mcpToolDefinitions() []any {
	return []any{
		map[string]any{
			"name":        ToolStatus,
			"description": "assistant daemon 的完整状态快照：目标仓库、待办队列、进行中的评审/分诊会话、最近会话结果。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name":        ToolListSessions,
			"description": "只列当前进行中的评审/分诊会话（含开始时间与已运行时长）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name":        ToolListQueue,
			"description": "只列各仓库最近一轮检测到的待办队列（review/triage 数量与明细）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name":        ToolRecentResults,
			"description": "最近结束的会话结果（默认 10 条，最多 100 条）。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "number", "description": "返回条数（默认 10）"},
				},
			},
		},
	}
}

// callTool 执行工具调用：全部是 daemon API 的只读查询。
func callTool(ctx context.Context, params json.RawMessage, getenv func(string) string) (string, error) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Limit int `json:"limit"`
		} `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &call); err != nil {
			return "", fmt.Errorf("解析工具参数: %w", err)
		}
	}
	client, err := Discover(getenv)
	if err != nil {
		return "", err
	}
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var value any
	switch call.Name {
	case ToolStatus:
		value, err = client.Status(queryCtx)
	case ToolListSessions:
		value, err = client.Sessions(queryCtx)
	case ToolListQueue:
		value, err = client.Queue(queryCtx)
	case ToolRecentResults:
		limit := call.Arguments.Limit
		if limit <= 0 {
			limit = 10
		}
		value, err = client.Results(queryCtx, limit)
	default:
		return "", fmt.Errorf("未知工具：%s", call.Name)
	}
	if err != nil {
		return "", err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
