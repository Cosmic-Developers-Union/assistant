package aigateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Gateway 是网关运行时：鉴权 → 协议/会话/工具识别 → 上游链故障转移 → 流式
// 回传（可选数据驻留归档）。请求体只在**必须改写**时才重新序列化（模型映射、
// prompt_cache_key / metadata 注入）；其余情况逐字节透传。
type Gateway struct {
	config   *Config
	resolver *SessionResolver
	client   *http.Client
	recorder *Recorder
	log      func(string, ...any)

	mu       sync.Mutex
	requests int64
	failures int64
	sources  map[string]int64
	tools    map[string]int64
	protocol map[string]int64
	upstream map[string]int64
}

// New 构造网关（配置需已 Normalize/Validate）；启用数据驻留但目录不可用时
// 返回错误（审计能力不静默降级）。
func New(config *Config, log func(string, ...any)) (*Gateway, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	gateway := &Gateway{
		config:   config,
		resolver: &SessionResolver{Secret: []byte(config.Session.Secret), HeaderNames: config.Session.Headers},
		client: &http.Client{
			Transport: http.DefaultTransport,
		},
		log:      log,
		sources:  map[string]int64{},
		tools:    map[string]int64{},
		protocol: map[string]int64{},
		upstream: map[string]int64{},
	}
	if config.Residency.Enabled {
		recorder, err := NewRecorder(config.Residency.Dir)
		if err != nil {
			return nil, err
		}
		gateway.recorder = recorder
	}
	return gateway, nil
}

// Handler 返回网关的 HTTP 路由。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses"} {
		mux.HandleFunc("POST "+path, g.handleProxy)
	}
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"backends":  BackendNames(),
			"protocols": SupportedProtocols(),
			"tools":     ToolNames(),
			"residency": g.recorder != nil,
		})
	})
	mux.HandleFunc("GET /status", g.handleStatus)
	return mux
}

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	protocol, ok := ProtocolForPath(r.URL.Path)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found_error", "未知端点："+r.URL.Path)
		return
	}
	key, ok := g.config.KeyFor(r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "authentication_error", "无效的网关令牌（Authorization: Bearer <token>）")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败："+err.Error())
		return
	}
	if len(raw) > MaxRequestBodyBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "请求体超过上限")
		return
	}
	// 解析只用于路由 / 会话 / 工具识别；转发默认用原始字节
	document := map[string]any{}
	if err := json.Unmarshal(raw, &document); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是 JSON："+err.Error())
		return
	}
	model, _ := document["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 model")
		return
	}
	if !key.AllowsModel(model) {
		writeAPIError(w, http.StatusForbidden, "permission_error", "密钥不允许访问模型 "+model)
		return
	}
	session := g.resolver.Resolve(r.Header, document, key.Name)
	tool := DetectTool(r.Header)
	chain := g.config.Candidates(model, protocol)
	if len(chain) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("没有匹配的模型路由：%s（协议 %s；检查 models 与后端类型支持的协议）", model, protocol))
		return
	}

	g.mu.Lock()
	g.requests++
	g.sources[session.Source]++
	g.protocol[protocol]++
	if tool != "" {
		g.tools[tool]++
	}
	g.mu.Unlock()

	var lastErr error
	for index, upstream := range chain {
		last := index == len(chain)-1
		session.Protocol = protocol
		record := g.beginRecord(r, key, session, tool, protocol, model, upstream)
		response, upstreamBody, err := g.attempt(r.Context(), upstream, r, raw, session, model, tool)
		if err != nil {
			lastErr = err
			g.finishRecord(record, ResponseMeta{Error: err.Error()})
			g.logAttempt(session, tool, key.Name, model, protocol, upstream, 0, 0, err)
			continue
		}
		if record != nil {
			_ = record.WriteBodies(raw, upstreamBody)
		}
		if !last && retryableStatus(response.StatusCode) {
			lastErr = fmt.Errorf("上游 %s 返回 %d", upstream.Name, response.StatusCode)
			g.finishRecord(record, ResponseMeta{Status: response.StatusCode, Headers: RedactRequestHeaders(response.Header)})
			g.logAttempt(session, tool, key.Name, model, protocol, upstream, response.StatusCode, 0, lastErr)
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			continue
		}
		g.mu.Lock()
		g.upstream[upstream.Name]++
		g.mu.Unlock()
		var sink io.Writer
		var sinkCloser io.Closer
		if record != nil {
			if closer, sinkErr := record.ResponseWriter(); sinkErr == nil {
				sink, sinkCloser = closer, closer
			} else {
				g.log("数据驻留写入失败：%v", sinkErr)
			}
		}
		written := streamResponse(w, response, sink)
		if sinkCloser != nil {
			_ = sinkCloser.Close()
		}
		g.finishRecord(record, ResponseMeta{
			Status:  response.StatusCode,
			Headers: RedactRequestHeaders(response.Header),
			Bytes:   written,
		})
		g.logAttempt(session, tool, key.Name, model, protocol, upstream, response.StatusCode, written, nil)
		return
	}
	g.mu.Lock()
	g.failures++
	g.mu.Unlock()
	message := "所有上游均失败"
	if lastErr != nil {
		message += "：" + lastErr.Error()
	}
	g.log("session=%s model=%s 失败：%s", session.ID, model, message)
	writeAPIError(w, http.StatusBadGateway, "api_error", message)
}

// attempt 向上游发起一次请求：模型映射与鉴权改写、会话注入、UA 规范化。
// 返回实际上游请求体（未改写时为原始字节）。
func (g *Gateway) attempt(
	parent context.Context,
	upstream ResolvedUpstream,
	original *http.Request,
	raw []byte,
	session Session,
	model string,
	tool string,
) (*http.Response, []byte, error) {
	header := original.Header.Clone()
	// 客户端凭据不进上游（上游用自身令牌）；其余头原样保留
	header.Del("Authorization")
	header.Del("x-api-key")
	for name, value := range upstream.Headers {
		header.Set(name, value)
	}
	NormalizeUserAgent(header, tool, g.config.UserAgent)

	// 请求体延迟解析：只有模型映射或后端特化真正写字段时才重新序列化，
	// 否则逐字节透传给上游
	body := NewBody(raw)
	if upstream.Model != "" && upstream.Model != model {
		body.Set("model", upstream.Model)
	}
	if upstream.Prepare != nil {
		upstream.Prepare(header, body, session)
	}
	upstreamBody, err := body.Encoded()
	if err != nil {
		return nil, nil, fmt.Errorf("序列化上游请求: %w", err)
	}
	auth := upstream.Auth
	if auth == "" {
		auth = "bearer"
	}
	switch auth {
	case "x-api-key":
		header.Set("x-api-key", upstream.Token)
	default:
		header.Set("Authorization", "Bearer "+upstream.Token)
	}

	target := upstream.BaseURL + original.URL.Path
	if original.URL.RawQuery != "" {
		target += "?" + original.URL.RawQuery
	}
	timeout := time.Duration(upstream.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Duration(DefaultUpstreamTimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	request, err := http.NewRequestWithContext(ctx, original.Method, target, bytes.NewReader(upstreamBody))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	request.Header = header
	response, err := g.client.Do(request)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("连接上游 %s: %w", upstream.Name, err)
	}
	response.Body = &cancelReadCloser{inner: response.Body, cancel: cancel}
	return response, upstreamBody, nil
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.config.KeyFor(r.Header.Get("Authorization"), r.Header.Get("x-api-key")); !ok {
		writeAPIError(w, http.StatusUnauthorized, "authentication_error", "无效的网关令牌")
		return
	}
	type modelEntry struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	models := g.config.ModelIDs()
	data := make([]modelEntry, 0, len(models))
	for _, model := range models {
		data = append(data, modelEntry{Type: "model", ID: model, DisplayName: model})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": false})
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.config.KeyFor(r.Header.Get("Authorization"), r.Header.Get("x-api-key")); !ok {
		writeAPIError(w, http.StatusUnauthorized, "authentication_error", "无效的网关令牌")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":   g.requests,
		"failures":   g.failures,
		"sources":    g.sources,
		"tools":      g.tools,
		"protocols":  g.protocol,
		"upstreams":  g.upstream,
		"backends":   BackendNames(),
		"user_agent": g.config.UserAgent,
		"residency":  g.recorder != nil,
	})
}

// beginRecord 创建数据驻留记录（未启用时返回 nil）。
func (g *Gateway) beginRecord(
	r *http.Request,
	key APIKey,
	session Session,
	tool, protocol, model string,
	upstream ResolvedUpstream,
) *Record {
	if g.recorder == nil {
		return nil
	}
	record, err := g.recorder.Begin(RequestMeta{
		Method:        r.Method,
		Path:          r.URL.Path,
		Protocol:      protocol,
		Model:         model,
		Upstream:      upstream.Name,
		Key:           key.Name,
		Tool:          tool,
		ToolVersion:   ToolVersion(tool, r.Header),
		Session:       session.ID,
		SessionSource: session.Source,
		Headers:       RedactRequestHeaders(r.Header),
	})
	if err != nil {
		g.log("数据驻留写入失败：%v", err)
		return nil
	}
	return record
}

func (g *Gateway) finishRecord(record *Record, meta ResponseMeta) {
	if record == nil {
		return
	}
	if err := record.Finish(meta); err != nil {
		g.log("数据驻留写入失败：%v", err)
	}
}

func (g *Gateway) logAttempt(session Session, tool, key, model, protocol string, upstream ResolvedUpstream, status int, bytes int64, err error) {
	upstreamModel := upstream.Model
	if upstreamModel == "" {
		upstreamModel = model
	}
	note := ""
	if err != nil {
		note = " error=" + err.Error()
	}
	toolNote := ""
	if tool != "" {
		toolNote = " tool=" + tool
	}
	g.log("session=%s source=%s%s key=%s protocol=%s model=%s upstream=%s upstream_model=%s status=%d bytes=%d%s",
		session.ID, session.Source, toolNote, key, protocol, model, upstream.Name, upstreamModel, status, bytes, note)
}

// retryableStatus 报告该状态是否值得故障转移到下一上游：网络类/限流/服务端
// 错误重试；4xx（客户端错误）直接回传给调用方，避免掩盖问题。
func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// streamResponse 把上游响应透传回客户端（含 SSE 流式），可选同时落盘（数据
// 驻留：上游响应体完整保留），返回写入客户端的字节数。
func streamResponse(w http.ResponseWriter, response *http.Response, sink io.Writer) int64 {
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	var written int64
	for {
		read, err := response.Body.Read(buffer)
		if read > 0 {
			if sink != nil {
				_, _ = sink.Write(buffer[:read])
			}
			count, writeErr := w.Write(buffer[:read])
			written += int64(count)
			if flusher != nil {
				flusher.Flush()
			}
			if writeErr != nil {
				return written
			}
		}
		if err != nil {
			return written
		}
	}
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(target, source http.Header) {
	for name, values := range source {
		if hopByHopHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = w.Write(append(encoded, '\n'))
}

// writeAPIError 按 Anthropic 错误结构回传，客户端能原样展示。
func writeAPIError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": message,
		},
	})
}

// cancelReadCloser 在响应体关闭时释放请求上下文。
type cancelReadCloser struct {
	inner  io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Read(buffer []byte) (int, error) { return c.inner.Read(buffer) }

func (c *cancelReadCloser) Close() error {
	err := c.inner.Close()
	c.cancel()
	return err
}

func constantTimeEqual(first, second string) bool {
	return subtle.ConstantTimeCompare([]byte(first), []byte(second)) == 1
}
