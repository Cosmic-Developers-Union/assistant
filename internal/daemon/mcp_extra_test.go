package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// TestRunMCPIgnoresNoiseAndUnknownMethod 钉住 stdio JSON-RPC 循环的容错契约：
// 空行、坏 JSON、以及没有 id 的通知都必须被忽略（stdio 上混入日志不致命），
// 未知方法必须回 -32601 而不是让循环退出——MCP 客户端探测新能力时会发未知
// 方法，直接断流会让整个会话的工具面消失。
func TestRunMCPIgnoresNoiseAndUnknownMethod(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	input := strings.Join([]string{
		"",
		"   ",
		"这不是 JSON",
		`{"jsonrpc":"2.0","method":"initialize"}`, // 无 id → 通知，不响应
		`{"jsonrpc":"2.0","id":null,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"nonexistent/method"}`,
	}, "\n") + "\n"

	var output strings.Builder
	if err := RunMCP(t.Context(), strings.NewReader(input), &output, os.Getenv, "v-mcp"); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	lines := nonEmptyLines(output.String())
	if len(lines) != 2 {
		t.Fatalf("只应响应 2 条（ping + 未知方法），实际 %d 条：%s", len(lines), output.String())
	}
	var ping map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ping); err != nil {
		t.Fatalf("ping 响应不是 JSON：%v", err)
	}
	if ping["id"] != float64(1) {
		t.Errorf("ping 响应 id = %v", ping["id"])
	}
	if _, ok := ping["error"]; ok {
		t.Errorf("ping 不应报错：%s", lines[0])
	}

	var unknown map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &unknown); err != nil {
		t.Fatalf("未知方法响应不是 JSON：%v", err)
	}
	errorObject, ok := unknown["error"].(map[string]any)
	if !ok || errorObject["code"] != float64(-32601) {
		t.Errorf("未知方法应回 -32601：%s", lines[1])
	}
}

// TestCallToolArgumentAndToolErrors 钉住 callTool 的三类失败与默认值：
// 参数不是 JSON（客户端 bug）、未知工具名、以及 daemon 未运行时 Discover 的
// 错误必须原样透给调用方（MCP 侧包成 isError 文本）。default 分支的 limit
// 缺省 10 是 recent_results 的公开契约。
func TestCallToolArgumentAndToolErrors(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(configDir, "config.json"))

	if _, err := callTool(t.Context(), json.RawMessage("{不是 JSON"), os.Getenv); err == nil ||
		!strings.Contains(err.Error(), "解析工具参数") {
		t.Errorf("坏参数应报「解析工具参数」：%v", err)
	}

	// 未知工具：Discover 先跑（必须能发现 daemon），再由 switch 的 default 拒绝。
	// 用一个只回 401 的桩端点点出「发现成功但工具名不认识」的场景。
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	endpointPath, err := instances.DaemonEndpointPath()
	if err != nil {
		t.Fatal(err)
	}
	writeEndpointFile(t, endpointPath, `{"addr":"`+strings.TrimPrefix(server.URL, "http://")+`","token":"tok"}`)

	if _, err := callTool(t.Context(), json.RawMessage(`{"name":"nope"}`), os.Getenv); err == nil ||
		!strings.Contains(err.Error(), "未知工具") {
		t.Errorf("未知工具应报「未知工具」：%v", err)
	}

	// daemon 未运行：Discover 失败，错误必须上抛（不能返回空字符串假装成功）
	t.Setenv(EndpointEnv, filepath.Join(t.TempDir(), "absent.json"))
	if _, err := callTool(t.Context(), json.RawMessage(`{"name":"`+ToolStatus+`"}`), os.Getenv); err == nil ||
		!strings.Contains(err.Error(), "先 assistant run") {
		t.Errorf("daemon 未运行时应上抛 Discover 错误：%v", err)
	}
}

// TestCallToolServesAllFourTools 钉住四个只读工具的端到端契约：全部走 daemon
// 的只读 API 并返回缩进 JSON。少接一个工具（如 recent_results 的 limit 缺省）
// 会让 MCP 客户端调用后拿到空结果却不报错。
func TestCallToolServesAllFourTools(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.RequestURI())
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/status":
			_ = json.NewEncoder(writer).Encode(Status{Version: "v-stub", Targets: []Target{{Host: "https://gitea.example.com", Repository: "acme/repo"}}})
		case "/api/v1/sessions":
			_ = json.NewEncoder(writer).Encode([]Session{{Host: "h", Repository: "r"}})
		case "/api/v1/queue":
			_ = json.NewEncoder(writer).Encode([]Queue{{Host: "h", Repository: "r"}})
		case "/api/v1/results":
			_ = json.NewEncoder(writer).Encode([]Result{{Host: "h", Repository: "r"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	endpointPath := filepath.Join(t.TempDir(), "daemon.json")
	writeEndpointFile(t, endpointPath, `{"addr":"`+strings.TrimPrefix(server.URL, "http://")+`","token":"tok-a"}`)
	getenv := func(key string) string {
		if key == EndpointEnv {
			return endpointPath
		}
		return ""
	}

	for _, name := range []string{ToolStatus, ToolListSessions, ToolListQueue, ToolRecentResults} {
		body := `{"name":"` + name + `"}`
		if name == ToolRecentResults {
			// limit<=0 必须缺省成 10
			body = `{"name":"` + name + `","arguments":{"limit":0}}`
		}
		result, err := callTool(t.Context(), json.RawMessage(body), getenv)
		if err != nil {
			t.Fatalf("callTool(%s): %v", name, err)
		}
		if !strings.Contains(result, "\n  ") {
			t.Errorf("%s 应返回缩进 JSON：%s", name, result)
		}
	}

	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "/api/v1/status") || !strings.Contains(joined, "/api/v1/sessions") ||
		!strings.Contains(joined, "/api/v1/queue") || !strings.Contains(joined, "/api/v1/results?limit=10") {
		t.Errorf("四个工具应各打一个只读端点（recent_results 缺省 limit=10）：%s", joined)
	}
}

// TestRunMCPToolCallErrorIsReturnedAsContent 钉住 tools/call 出错时的响应形态：
// 必须包成 content + isError=true（而不是 JSON-RPC error），否则 MCP 客户端会
// 当成传输层故障中断整轮会话，而不是把「daemon 未运行」告诉模型。
func TestRunMCPToolCallErrorIsReturnedAsContent(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + ToolStatus + `"}}` + "\n"
	var output strings.Builder
	if err := RunMCP(t.Context(), strings.NewReader(input), &output, os.Getenv, "v"); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(nonEmptyLines(output.String())[0]), &response); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if response.Error != nil {
		t.Errorf("工具失败不能走 JSON-RPC error：%s", output.String())
	}
	if !response.Result.IsError || len(response.Result.Content) == 0 ||
		!strings.Contains(response.Result.Content[0].Text, "先 assistant run") {
		t.Errorf("工具失败应回 content+isError 并带原因：%s", output.String())
	}
}

func nonEmptyLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}
