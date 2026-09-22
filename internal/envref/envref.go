// Package envref 统一 config.json 密钥字段的环境变量引用展开：$VAR、${VAR}、
// ${VAR:-default}。
//
// 语义（严格，fail fast）：
//   - 变量名 [A-Za-z_][A-Za-z0-9_]*；引用未定义的变量是错误——密钥发出去的
//     是字面量 "$VAR" 还是空串都难以排查，宁可启动即报错；
//   - "${VAR:-}" 是显式空缺省的逃生门（变量未定义或为空串时取缺省段）；
//     缺省段允许再引用变量（例：${XDG_DATA_HOME:-$HOME/.local/share}）；
//   - "$" 后不构成合法变量名（如 "$5"、孤立 "$"）按字面量保留，兼容含 "$"
//     的密钥；
//   - 未闭合的 "${"、花括号内为空是语法错误。
//
// 展开只发生在消费点（构造通道客户端、写出 settings/mcp 配置等）；File 里的
// 原始配置永远保留引用形式，保存时不会被洗成明文。
package envref

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// Lookup 是环境变量读取注入点；nil 时用 os.LookupEnv（测试注入假环境用）。
type Lookup func(name string) (value string, ok bool)

// Options 是展开选项：Lookup 注入环境读取，Field 是报错信息里的字段路径
// （如 "channels[qq/support].app_secret"）。
type Options struct {
	Lookup Lookup
	Field  string
}

// UndefinedError 是引用了未定义环境变量的错误（errors.As 取 Name 做提示）。
type UndefinedError struct {
	Field string
	Name  string
}

func (e *UndefinedError) Error() string {
	return fmt.Sprintf("%s: 引用了未定义的环境变量 %s（在 config.json 同目录的 .env 或进程环境中定义；允许为空写 ${%s:-}）",
		e.field(), e.Name, e.Name)
}

func (e *UndefinedError) field() string {
	return cmp.Or(e.Field, "配置值")
}

// SyntaxError 是引用语法错误（未闭合的 "${"、空的或非法的变量名）。
type SyntaxError struct {
	Field  string
	Detail string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("%s: %s", cmp.Or(e.Field, "配置值"), e.Detail)
}

// Expand 展开 value 里的全部变量引用；引用未定义变量或语法非法时返回
// *UndefinedError / *SyntaxError。
func Expand(value string, options Options) (string, error) {
	lookup := options.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	field := cmp.Or(options.Field, "配置值")
	var builder strings.Builder
	for {
		dollar := strings.IndexByte(value, '$')
		if dollar < 0 {
			builder.WriteString(value)
			return builder.String(), nil
		}
		builder.WriteString(value[:dollar])
		rest := value[dollar+1:]
		if strings.HasPrefix(rest, "{") {
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				return "", &SyntaxError{Field: field, Detail: fmt.Sprintf("未闭合的 ${ 引用：%q", "${"+rest)}
			}
			name, defaultValue, hasDefault := strings.Cut(rest[1:end], ":-")
			name = strings.TrimSpace(name)
			if name == "" || !validName(name) {
				return "", &SyntaxError{Field: field, Detail: fmt.Sprintf("非法变量名：%q（形如 ${VAR} 或 ${VAR:-default}）", rest[:end+1])}
			}
			resolved, ok := lookup(name)
			if (!ok || resolved == "") && hasDefault {
				// 缺省段里允许再引用变量；嵌套沿用同一环境与字段路径
				expanded, err := Expand(defaultValue, Options{Lookup: lookup, Field: field})
				if err != nil {
					return "", err
				}
				resolved = expanded
			} else if !ok {
				return "", &UndefinedError{Field: field, Name: name}
			}
			builder.WriteString(resolved)
			value = rest[end+1:]
			continue
		}
		// 裸 $VAR：取最长合法变量名；不构成名字则按字面量保留 "$"
		name := scanName(rest)
		if name == "" {
			builder.WriteByte('$')
			value = rest
			continue
		}
		resolved, ok := lookup(name)
		if !ok {
			return "", &UndefinedError{Field: field, Name: name}
		}
		builder.WriteString(resolved)
		value = rest[len(name):]
	}
}

// Validate 只做引用语法与变量已定义检查（装载期 fail fast 用），不产出展开值。
func Validate(value string, options Options) error {
	_, err := Expand(value, options)
	return err
}

// ExpandEnvMap 展开 map 的值（键不动），返回新 map；入参不被修改。字段路径
// 逐项标注为 "<field>.env[KEY]"，便于定位是哪个变量没定义。
func ExpandEnvMap(env map[string]string, options Options) (map[string]string, error) {
	if env == nil {
		return nil, nil
	}
	expanded := make(map[string]string, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		value, err := Expand(env[key], Options{
			Lookup: options.Lookup,
			Field:  fmt.Sprintf("%s.env[%s]", options.Field, key),
		})
		if err != nil {
			return nil, err
		}
		expanded[key] = value
	}
	return expanded, nil
}

// ExpandMCPEnv 深拷贝遍历 any 树（MCP server 定义），只展开键名为 "env" 的
// map 里的字符串值（如 agents.<name>.mcp.<server>.env.<KEY>）；其余原样返回，
// 入参不被修改。
func ExpandMCPEnv(value any, options Options) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		field := cmp.Or(options.Field, "mcp")
		copied := make(map[string]any, len(typed))
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			item := typed[key]
			childField := fmt.Sprintf("%s.%s", field, key)
			if key == "env" {
				expanded, err := expandMCPEnvValues(item, Options{Lookup: options.Lookup, Field: childField})
				if err != nil {
					return nil, err
				}
				copied[key] = expanded
				continue
			}
			child, err := ExpandMCPEnv(item, Options{Lookup: options.Lookup, Field: childField})
			if err != nil {
				return nil, err
			}
			copied[key] = child
		}
		return copied, nil
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			child, err := ExpandMCPEnv(item, options)
			if err != nil {
				return nil, err
			}
			copied[index] = child
		}
		return copied, nil
	default:
		return value, nil
	}
}

// expandMCPEnvValues 展开 MCP server 定义里 env 段的字符串值；非字符串值
// （容错的数字/布尔）原样保留。
func expandMCPEnvValues(value any, options Options) (any, error) {
	envMap, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}
	copied := make(map[string]any, len(envMap))
	for _, key := range slices.Sorted(maps.Keys(envMap)) {
		item := envMap[key]
		text, isString := item.(string)
		if !isString {
			copied[key] = item
			continue
		}
		expanded, err := Expand(text, Options{
			Lookup: options.Lookup,
			Field:  fmt.Sprintf("%s.%s", options.Field, key),
		})
		if err != nil {
			return nil, err
		}
		copied[key] = expanded
	}
	return copied, nil
}

// validName 判断合法变量名（[A-Za-z_][A-Za-z0-9_]*）。
func validName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		switch {
		case r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'):
		case '0' <= r && r <= '9':
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// scanName 从 text 开头取最长合法变量名；没有返回空串。
func scanName(text string) string {
	end := 0
	for end < len(text) {
		r := text[end]
		switch {
		case r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'):
			end++
		case '0' <= r && r <= '9':
			if end == 0 {
				return ""
			}
			end++
		default:
			return text[:end]
		}
	}
	return text[:end]
}
