package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
)

// 展开形态（ProgressVerbose）的展示上限：日志一条一行，长结构按缩进 JSON 展开
// 但要封顶，否则一次工具调用就能把日志淹掉。
const (
	toolInputLineLimit  = 12
	toolResultLineLimit = 8
	toolLineWidth       = 200
)

// 紧凑形态（ProgressTerse）的截断长度：摘要只用来判断「这一步在干什么」。
const (
	toolInputDetailLimit  = 120
	toolResultDetailLimit = 160
)

// ThinkingProgressInterval 是思考进度的时间窗：thinking_tokens 每几十毫秒一条，
// 按时间窗播报才既能看到模型在动又不刷屏。
var ThinkingProgressInterval = 2 * time.Second

// thinkingBurstDelta 是时间窗内允许的额外播报阈值（词元数跳变很大时也报一次）。
const thinkingBurstDelta = 2_000

// Now 是可替换的时钟（测试注入固定时间以验证节流，避免 sleep 赌时序）。
var Now = time.Now

// thinkingReport 是上次播报的水位。
type thinkingReport struct {
	reportedAt time.Time
	reported   int
}

// thinkingThrottle 是思考播报的节流水位。放在包级是因为「上一帧」跨行累积，而
// Feed 是无状态函数；同一进程内并发的会话各有自己的 outcome，水位共用只影响播报
// 频率（不影响归集结果），可接受。
type thinkingThrottle struct {
	mu    sync.Mutex
	state thinkingReport
}

func (t *thinkingThrottle) get() thinkingReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *thinkingThrottle) set(report thinkingReport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = report
}

// thinkingState 是全局节流水位（零值即「从未播报」，首帧必然立即播报一次）。
var thinkingState thinkingThrottle

// DescribeToolInput 提炼工具入参里最有判断价值的一段（路径/编号/命令/标题等），
// 让日志读者不打开原始记录也能感知「这一步在干什么」。只取摘要，长值截断。
func DescribeToolInput(name string, raw json.RawMessage) string {
	return describeToolInput(name, raw)
}

func describeToolInput(_ string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	// 按工具常见的关键字段优先取；取不到就汇总前几个键
	keys := []string{"command", "file_path", "path", "pattern", "url", "query", "description",
		"repository", "repo", "owner", "number", "index", "title", "body", "state", "name", "prompt"}
	var parts []string
	for _, key := range keys {
		value, ok := input[key]
		if !ok {
			continue
		}
		text := strings.TrimSpace(fmt.Sprintf("%v", value))
		if text == "" {
			continue
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(text); len(runes) > toolInputDetailLimit {
			text = string(runes[:toolInputDetailLimit]) + "…"
		}
		switch key {
		case "body", "prompt":
			// 长文本只报长度语义，不刷屏
			parts = append(parts, fmt.Sprintf("%s(%d 字)", key, len([]rune(text))))
		default:
			parts = append(parts, text)
		}
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// DescribeToolResult 折叠 tool_result：错误要显出来，长输出只报规模。
func DescribeToolResult(block *ContentBlock) string {
	return describeToolResult(block)
}

func describeToolResult(block *ContentBlock) string {
	if block.IsError != nil && *block.IsError {
		text := strings.TrimSpace(block.Text)
		if text == "" && len(block.Content) > 0 {
			text = strings.TrimSpace(string(block.Content))
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(text); len(runes) > toolResultDetailLimit {
			text = string(runes[:toolResultDetailLimit]) + "…"
		}
		return "✗ " + text
	}
	if len(block.Content) > 0 {
		return fmt.Sprintf("（返回 %d 字）", len(block.Content))
	}
	if block.Text != "" {
		return fmt.Sprintf("（返回 %d 字）", len([]rune(block.Text)))
	}
	return "（完成）"
}

// ToolUseLines 把工具调用展开成多行（展开形态）：工具名一行，入参按缩进 JSON
// 逐行展开并封顶。
func ToolUseLines(name string, raw json.RawMessage) string {
	lines := []string{"🔧 " + name}
	if len(bytes.TrimSpace(raw)) == 0 {
		return lines[0]
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		lines = append(lines, "  "+truncateRunes(singleLine(string(raw)), toolLineWidth))
		return strings.Join(lines, "\n")
	}
	for _, line := range claudecfg.JSONLines(value, toolInputLineLimit) {
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n")
}

// toolResultText 从 tool_result 的 content 里取文本（可能是字符串或内容块数组）。
func toolResultText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return string(raw)
}

// describeInit 把 init 事件整理成**逐行**会话实况：每行一个字段，日志里不出现
// 长行（版本/认证/权限/工作目录/工具面/MCP 连通状态）。
func describeInit(event Event) string {
	lines := []string{"会话已启动"}
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, "  "+label+" = "+value)
		}
	}
	add("model", pointerOr(event.Model, ""))
	add("claude", pointerOr(event.ClaudeCodeVersion, ""))
	add("认证", pointerOr(event.APIKeySource, ""))
	add("权限", pointerOr(event.PermissionMode, ""))
	add("工作目录", pointerOr(event.Cwd, ""))
	if len(event.Tools) > 0 {
		add("工具", itoa(len(event.Tools))+" 个")
	}
	if len(event.Skills) > 0 {
		add("技能", itoa(len(event.Skills))+" 个")
	}
	if len(event.Agents) > 0 {
		add("子代理", itoa(len(event.Agents))+" 个")
	}
	if len(event.MCPServers) > 0 {
		servers := make([]string, 0, len(event.MCPServers))
		for _, server := range event.MCPServers {
			servers = append(servers, server.Name+"="+server.Status)
		}
		add("MCP", strings.Join(servers, " "))
	}
	return strings.Join(lines, "\n")
}

// BrokenMCPServers 返回未就绪的 MCP server 名单（name=status，空格分隔）。
// init 事件里声明了但没连上的 server 是「工具用不了」的最常见原因，必须显式提示。
func BrokenMCPServers(outcome *Outcome) string {
	if len(outcome.MCPStatus) == 0 {
		return ""
	}
	names := make([]string, 0, len(outcome.MCPStatus))
	for name, status := range outcome.MCPStatus {
		if status == "connected" || status == "" {
			continue
		}
		names = append(names, name+"="+status)
	}
	sortedStrings(names)
	return strings.Join(names, " ")
}

// truncateRunes 按 rune 截断（避免劈开多字节字符）。
func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// itoa 是 strconv.Itoa 的短别名（本文件多处拼接数字，省略 import 噪音）。
func itoa(value int) string { return strconv.Itoa(value) }

// textLines 把文本按行限与宽度折成若干展示行。
func textLines(text string, lineLimit, width int) []string {
	return claudecfg.TextLines(text, lineLimit, width)
}

// sortedStrings 就地排序（小切片冒泡即可，避免为几项引入 sort 的开销语义）。
func sortedStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// envOrFallback 取环境变量，缺省时用回退值（供 SessionEnv 与探测共用）。
func envOrFallback(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
