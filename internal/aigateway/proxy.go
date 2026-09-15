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

// Gateway 是网关运行时：鉴权 → 会话解析 → 上游链故障转移 → 流式回传。
type Gateway struct {
	config   *Config
	resolver *SessionResolver
	client   *http.Client
	log      func(string, ...any)

	mu       sync.Mutex
	requests int64
	failures int64
	sources  map[string]int64
	upstream map[string]int64
}

// New 构造网关（配置需已 Normalize/Validate）。
func New(config *Config, log func(string, ...any)) *Gateway {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Gateway{
		config:   config,
		resolver: &SessionResolver{Secret: []byte(config.Session.Secret), HeaderNames: config.Session.Headers},
		client: &http.Client{
			Transport: http.DefaultTransport,
		},
		log:      log,
		sources:  map[string]int64{},
		upstream: map[string]int64{},
	}
}

// Handler 返回网关的 HTTP 路由。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	handleMessages := func(w http.ResponseWriter, r *http.Request) { g.handleMessages(w, r) }
	mux.HandleFunc("POST /v1/messages", handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", handleMessages)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "upstreams": len(g.config.Upstreams)})
	})
	mux.HandleFunc("GET /status", g.handleStatus)
	return mux
}

func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
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
	chain := g.config.UpstreamsFor(model)
	if len(chain) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "没有匹配的模型路由："+model)
		return
	}

	g.mu.Lock()
	g.requests++
	g.sources[session.Source]++
	g.mu.Unlock()

	var lastErr error
	for index, upstream := range chain {
		last := index == len(chain)-1
		response, err := g.attempt(r.Context(), upstream, r, raw, session, model)
		if err != nil {
			lastErr = err
			g.logAttempt(session, key.Name, model, upstream, 0, 0, err)
			continue
		}
		if !last && retryableStatus(response.StatusCode) {
			lastErr = fmt.Errorf("上游 %s 返回 %d", upstream.Name, response.StatusCode)
			g.logAttempt(session, key.Name, model, upstream, response.StatusCode, 0, lastErr)
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			continue
		}
		g.mu.Lock()
		g.upstream[upstream.Name]++
		g.mu.Unlock()
		written := streamResponse(w, response)
		g.logAttempt(session, key.Name, model, upstream, response.StatusCode, written, nil)
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

// attempt 向上游发起一次请求：重写模型名与鉴权、注入会话、附加静态头。
func (g *Gateway) attempt(
	parent context.Context,
	upstream Upstream,
	original *http.Request,
	raw []byte,
	session Session,
	model string,
) (*http.Response, error) {
	document := map[string]any{}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("解析请求体: %w", err)
	}
	if mapped := upstream.Models[model]; mapped != "" {
		document["model"] = mapped
	}
	header := original.Header.Clone()
	// 客户端凭据不进上游（上游用自身令牌）
	header.Del("Authorization")
	header.Del("x-api-key")
	for name, value := range upstream.Headers {
		header.Set(name, value)
	}
	upstream.Session.Apply(header, document, session.ID)
	switch upstream.Auth {
	case "x-api-key":
		header.Set("x-api-key", upstream.Token)
	default:
		header.Set("Authorization", "Bearer "+upstream.Token)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("序列化上游请求: %w", err)
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
	request, err := http.NewRequestWithContext(ctx, original.Method, target, bytes.NewReader(encoded))
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header = header
	response, err := g.client.Do(request)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("连接上游 %s: %w", upstream.Name, err)
	}
	// 取消要在响应体读完后进行：包一层
	response.Body = &cancelReadCloser{inner: response.Body, cancel: cancel}
	return response, nil
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
	data := make([]modelEntry, 0, len(g.config.Routes))
	for model := range g.config.Routes {
		if model == "*" {
			continue
		}
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
		"requests":  g.requests,
		"failures":  g.failures,
		"sources":   g.sources,
		"upstreams": g.upstream,
	})
}

func (g *Gateway) logAttempt(session Session, key, model string, upstream Upstream, status int, bytes int64, err error) {
	upstreamModel := upstream.Models[model]
	if upstreamModel == "" {
		upstreamModel = model
	}
	note := ""
	if err != nil {
		note = " error=" + err.Error()
	}
	g.log("session=%s source=%s key=%s model=%s upstream=%s upstream_model=%s status=%d bytes=%d%s",
		session.ID, session.Source, key, model, upstream.Name, upstreamModel, status, bytes, note)
}

// retryableStatus 报告该状态是否值得故障转移到下一上游：网络类/限流/服务端
// 错误重试；4xx（客户端错误）直接回传给调用方，避免掩盖问题。
func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// streamResponse 把上游响应透传回客户端（含 SSE 流式），返回写入字节数。
func streamResponse(w http.ResponseWriter, response *http.Response) int64 {
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	var written int64
	for {
		read, err := response.Body.Read(buffer)
		if read > 0 {
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

// writeAPIError 按 Anthropic 错误结构回传，客户端（Claude Code）能原样展示。
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
