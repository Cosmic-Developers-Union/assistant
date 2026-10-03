package claude

import (
	"encoding/json"
	"fmt"
	"strings"
)

func jsonLines(value any, limit int) []string {
	if value == nil {
		return nil
	}
	encoded, err := json.MarshalIndent(maskJSONValue(value), "", "  ")
	if err != nil {
		return nil
	}
	raw := strings.Split(strings.TrimRight(string(encoded), "\n"), "\n")
	if limit > 0 && len(raw) > limit {
		return append(raw[:limit:limit], fmt.Sprintf("…（共 %d 行，已截断）", len(raw)))
	}
	return raw
}

// TextLines 把多行文本压成逐行展示（空行丢弃，超长行按 limit 截断）。
func formatTextLines(text string, lineLimit, width int) []string {
	lines := make([]string, 0, 4)
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if width > 0 {
			line = truncateRunes(line, width)
		}
		lines = append(lines, line)
		if lineLimit > 0 && len(lines) >= lineLimit {
			lines = append(lines, "…（已截断）")
			break
		}
	}
	return lines
}

// MaskJSONValue 递归地把 JSON 值里「键名像密钥」的字符串打码（日志打印工具入参这类
// 任意结构时用），其余原样返回；数组与嵌套对象一并处理。
func maskJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if isSecretKey(key) {
				if text, ok := item.(string); ok {
					result[key] = maskSecret(text)
					continue
				}
			}
			result[key] = maskJSONValue(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, maskJSONValue(item))
		}
		return result
	default:
		return value
	}
}

func isSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	return strings.Contains(upper, "TOKEN") || strings.Contains(upper, "KEY") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD")
}
func maskSecret(value string) string {
	if len(value) <= 10 {
		return "***"
	}
	return value[:6] + "…" + value[len(value)-4:]
}
