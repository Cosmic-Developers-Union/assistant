package sessionstore

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ExtractMessages 从一行原始 jsonl 里抽取可读消息（claude 的文本记录格式）：
//
//	{"type":"user","message":{"role":"user","content":"你好"}}
//	{"type":"assistant","message":{"content":[{"type":"text","text":"…"},
//	    {"type":"thinking","thinking":"…"},{"type":"tool_use","name":"Bash"}]}}
//	{"type":"user","message":{"content":[{"type":"tool_result","content":"…"}]}}
//	{"type":"system","subtype":"api_error","error":{"message":"…"}}
//
// 元数据行（last-prompt / attachment / queue-operation 等）返回空——它们的文本会
// 与真正的消息重复。一行可能含多条消息（content 数组），所以返回切片。
func ExtractMessages(line []byte, lineNumber int) []Message {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var document struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &document); err != nil {
		return nil
	}
	appendMessage := func(messages []Message, role, text string) []Message {
		text = strings.TrimSpace(text)
		if text == "" {
			return messages
		}
		return append(messages, Message{Line: lineNumber, Role: role, Text: text, Timestamp: document.Timestamp})
	}
	messages := make([]Message, 0, 2)
	switch document.Type {
	case "user", "assistant":
		var text string
		if err := json.Unmarshal(document.Message.Content, &text); err == nil {
			role := document.Type
			return appendMessage(messages, role, text)
		}
		var blocks []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(document.Message.Content, &blocks); err != nil {
			return nil
		}
		for _, block := range blocks {
			switch block.Type {
			case "text":
				messages = appendMessage(messages, document.Type, block.Text)
			case "thinking":
				messages = appendMessage(messages, "thinking", block.Thinking)
			case "tool_use":
				messages = appendMessage(messages, "tool", block.Name+" "+compactJSON(block.Input))
			case "tool_result":
				messages = appendMessage(messages, "tool_result", toolResultText(block.Content))
			}
		}
	case "system":
		if document.Subtype == "api_error" {
			messages = appendMessage(messages, "error", document.Error.Message)
		}
	case "summary":
		var summary struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(trimmed, &summary); err == nil {
			messages = appendMessage(messages, "summary", summary.Summary)
		}
	}
	return messages
}

// toolResultText 取 tool_result 的文本（字符串或内容块数组）。
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
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, " ")
}

// compactJSON 把工具入参压成一行短文本（抽取文本用，不做脱敏——入库的是原始记录）。
func compactJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	text := string(encoded)
	if runes := []rune(text); len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return text
}

// Summarize 汇总一条记录的检索摘要：首条 user 文本与最后一条 assistant 文本。
func Summarize(lines []string) (firstUser, lastAssistant string) {
	for index, line := range lines {
		for _, message := range ExtractMessages([]byte(line), index) {
			if firstUser == "" && message.Role == "user" {
				firstUser = snippet(message.Text, 200)
			}
			if message.Role == "assistant" {
				lastAssistant = snippet(message.Text, 200)
			}
		}
	}
	return firstUser, lastAssistant
}

func snippet(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// DetectSource 从项目名推断会话用途（仅在记录没有会话元数据时的兜底）：
// 聊天会话的项目名由工作目录派生（形如 assistant-chat 或 …-chat-chat-940d3155），
// 评审/分诊会话的项目名由调度器生成（assistant-<host>-<owner>-<repo>）。
func DetectSource(project string) string {
	lower := strings.ToLower(project)
	switch {
	case strings.Contains(lower, "chat"):
		return "chat"
	case strings.HasPrefix(lower, "assistant-"):
		return "review"
	case strings.TrimSpace(project) == "":
		return ""
	default:
		return "unknown"
	}
}
