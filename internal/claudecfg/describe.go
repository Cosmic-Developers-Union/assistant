package claudecfg

import (
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

// DescribeEnv 返回可读的环境变量摘要：KEY=VALUE 按名排序，密钥打码。
func DescribeEnv(env map[string]string) string {
	if len(env) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+MaskEnvValue(key, env[key]))
	}
	return strings.Join(pairs, " ")
}

// Describe 返回会话生效覆盖的一行摘要：env（密钥打码）、settings 键名、MCP 个数。
// settings 值不打印（可能含密钥/命令），只列键名，够定位问题。
func (o Overrides) Describe() string {
	parts := []string{fmt.Sprintf("env %d 项（%s）", len(o.Env), DescribeEnv(o.Env))}
	if len(o.Settings) == 0 {
		parts = append(parts, "settings 0 项")
	} else {
		keys := make([]string, 0, len(o.Settings))
		for key := range o.Settings {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts = append(parts, fmt.Sprintf("settings %d 项（%s，值不打印）", len(keys), strings.Join(keys, ",")))
	}
	parts = append(parts, fmt.Sprintf("mcp %d 个", len(o.MCP)))
	return strings.Join(parts, "；")
}

// DescribeMCPServers 摘要一份 MCP 配置文档（mcpServers 形态）：名字、命令与参数、
// env 键值（密钥打码）——「声明了什么 MCP」一眼可见；实际是否连通由会话 init 事件
// 的 mcp_servers 状态给出。
func DescribeMCPServers(document map[string]any) string {
	servers, _ := document["mcpServers"].(map[string]any)
	if len(servers) == 0 {
		return "无"
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		server, _ := servers[name].(map[string]any)
		if server == nil {
			parts = append(parts, name)
			continue
		}
		detail := ""
		if command, _ := server["command"].(string); command != "" {
			detail = command
			if args, ok := server["args"].([]any); ok && len(args) > 0 {
				values := make([]string, 0, len(args))
				for _, arg := range args {
					values = append(values, fmt.Sprintf("%v", arg))
				}
				detail += " " + strings.Join(values, " ")
			}
		}
		if env, ok := server["env"].(map[string]any); ok && len(env) > 0 {
			pairs := make([]string, 0, len(env))
			keys := make([]string, 0, len(env))
			for key := range env {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				pairs = append(pairs, key+"="+MaskEnvValue(key, fmt.Sprintf("%v", env[key])))
			}
			detail += " [" + strings.Join(pairs, " ") + "]"
		}
		if url, _ := server["url"].(string); url != "" {
			detail = url
		}
		if detail == "" {
			parts = append(parts, name)
			continue
		}
		parts = append(parts, name+"（"+detail+"）")
	}
	return strings.Join(parts, "；")
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
