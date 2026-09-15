package claudecfg

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MaskSecret 隐藏密钥中间部分：保留前 6 位与后 4 位，便于对着站点核对是哪一把；
// 太短的值整体打码（长度本身也是信息，所以仍然打码）。
func MaskSecret(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) <= 12 {
		return "***"
	}
	return string(runes[:6]) + "…" + string(runes[len(runes)-4:])
}

// secretMarkers 是「这个键承载密钥」的判断依据（大小写不敏感的子串）。
var secretMarkers = []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "COOKIE"}

// IsSecretKey 判断环境变量/配置键名是否承载密钥（决定要不要打码）。
func IsSecretKey(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range secretMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// MaskEnvValue 按变量名决定是否打码：密钥类只留首尾，其余原样（端点、模型名、
// 超时这些正是调试需要的）。
func MaskEnvValue(name, value string) string {
	if IsSecretKey(name) {
		return MaskSecret(value)
	}
	return value
}

// EnvLines 返回环境变量逐行展示：`KEY = VALUE` 按名排序，密钥打码。日志一条一行，
// 不要把 env 压成一条长行。
func EnvLines(env map[string]string) []string {
	if len(env) == 0 {
		return []string{"（无）"}
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+" = "+MaskEnvValue(key, env[key]))
	}
	return lines
}

// SettingKeys 返回 settings 的键名（值不打印：可能含密钥或命令），排序。
func SettingKeys(settings map[string]any) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// MCPServerLines 把一份 MCP 配置文档（mcpServers 形态）展开成逐行说明：
//
//	daemon：assistant mcp daemon
//	MiniMax：uvx minimax-coding-plan-mcp
//	  env.MINIMAX_API_KEY = sk-cp-…klmn
//
// 「声明了什么 MCP」一眼可见；实际是否连上由会话 init 事件的 mcp_servers 状态给出。
func MCPServerLines(document map[string]any) []string {
	servers, _ := document["mcpServers"].(map[string]any)
	if len(servers) == 0 {
		return []string{"（无）"}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		server, _ := servers[name].(map[string]any)
		if server == nil {
			lines = append(lines, name)
			continue
		}
		detail := ""
		if url, _ := server["url"].(string); url != "" {
			detail = url
		} else if command, _ := server["command"].(string); command != "" {
			detail = command
			if args, ok := server["args"].([]any); ok {
				for _, arg := range args {
					detail += " " + fmt.Sprintf("%v", arg)
				}
			}
		}
		lines = append(lines, name+"："+detail)
		if env, ok := server["env"].(map[string]any); ok && len(env) > 0 {
			keys := make([]string, 0, len(env))
			for key := range env {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				lines = append(lines, "  env."+key+" = "+MaskEnvValue(key, fmt.Sprintf("%v", env[key])))
			}
		}
	}
	return lines
}

// JSONLines 把任意 JSON 值渲染成缩进的多行文本（密钥字段打码），供日志逐行展示；
// limit 限制总行数，超出时以 `…（共 N 行）` 收尾。空值时返回 nil。
func JSONLines(value any, limit int) []string {
	if value == nil {
		return nil
	}
	encoded, err := json.MarshalIndent(MaskJSONValue(value), "", "  ")
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
func TextLines(text string, lineLimit, width int) []string {
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

// truncateRunes 按字符截断。
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// MaskJSONValue 递归地把 JSON 值里「键名像密钥」的字符串打码（日志打印工具入参这类
// 任意结构时用），其余原样返回；数组与嵌套对象一并处理。
func MaskJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if IsSecretKey(key) {
				if text, ok := item.(string); ok {
					result[key] = MaskSecret(text)
					continue
				}
			}
			result[key] = MaskJSONValue(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, MaskJSONValue(item))
		}
		return result
	default:
		return value
	}
}
