package aigateway

// 后端类型：一个后端一个文件（backend_opencode.go 等）。标准后端（standard）
// 不做任何特化——直接透传，端点与凭据来自配置；非标准后端把厂商差异放进代码：
// 端点、鉴权风格、会话头注入、请求体字段等，配置里只写一个类型名。
//
// 配置里 models[].backend 可以写：
//   - 内置类型名（opencode-go / zhipu / anthropic / openai ...）；
//   - "standard"：标准后端，必须给 base_url；
//   - backends 里自定义的命名后端（type 缺省 standard，也可指向内置类型）。

import (
	"net/http"
	"sort"
	"strings"
)

// Backend 是内置后端类型。
type Backend struct {
	Name    string
	Aliases []string
	// BaseURL 是内置端点（standard 为空：由配置提供）
	BaseURL string
	// Auth 是鉴权风格：bearer（Authorization: Bearer，缺省）或 x-api-key
	Auth string
	// Protocols 是该后端支持的协议（配置里 protocol 必须是其中之一）
	Protocols []string
	// Headers 是该后端的附加静态请求头（可选）
	Headers map[string]string
	// TimeoutMS 是该后端的缺省超时（可选）
	TimeoutMS int64
	// Prepare 是厂商特化：就地调整上游请求头与延迟解析的请求体；nil = 纯透传
	Prepare func(header http.Header, body *Body, session Session)
}

var backends = map[string]Backend{}

// RegisterBackend 注册内置后端（主名与别名，不区分大小写；重复 panic）。
func RegisterBackend(backend Backend) {
	if strings.TrimSpace(backend.Name) == "" {
		panic("aigateway: RegisterBackend 需要非空 Name")
	}
	for _, name := range append([]string{backend.Name}, backend.Aliases...) {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			panic("aigateway: 后端名字不能为空")
		}
		if _, exists := backends[key]; exists {
			panic("aigateway: 后端名重复：" + name)
		}
		backends[key] = backend
	}
}

// LookupBackend 按名查找内置后端类型。
func LookupBackend(name string) (Backend, bool) {
	backend, ok := backends[strings.ToLower(strings.TrimSpace(name))]
	return backend, ok
}

// BackendNames 返回内置后端类型名（排序，诊断/文档用）。
func BackendNames() []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(backends))
	for _, backend := range backends {
		if seen[backend.Name] {
			continue
		}
		seen[backend.Name] = true
		names = append(names, backend.Name)
	}
	sort.Strings(names)
	return names
}

// backendSupportsProtocol 报告后端类型是否支持该协议（standard 支持全部）。
func backendSupportsProtocol(backend Backend, protocol string) bool {
	if backend.Name == "" { // standard：由使用者保证
		return true
	}
	if len(backend.Protocols) == 0 {
		return true
	}
	for _, candidate := range backend.Protocols {
		if candidate == protocol {
			return true
		}
	}
	return false
}
