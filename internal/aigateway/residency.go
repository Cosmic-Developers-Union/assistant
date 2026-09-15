package aigateway

// 数据驻留（data residency）：完整保留每个请求与响应，供审计与排障。
//
// 归档布局（按请求一个目录，流式响应边收边写，内存占用与响应大小无关）：
//
//	<dir>/<YYYY-MM-DD>/<时间>-<随机>/
//	  request.meta.json        请求元信息（头已脱敏；session/tool/key/protocol/model）
//	  client-request.body      客户端原始请求体（逐字节）
//	  upstream-request.body    改写后的上游请求体（仅在改写时写）
//	  response.meta.json       响应元信息（状态/头/字节数/耗时/错误）
//	  upstream-response.body   上游响应体（SSE 也完整落盘）
//
// 元信息里不落任何凭据：Authorization / x-api-key / Cookie 等头统一脱敏。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Recorder 是数据驻留记录器（并发安全：每个请求独立目录与文件）。
type Recorder struct {
	dir string
	now func() time.Time
}

// NewRecorder 创建记录器（确保根目录可写）。
func NewRecorder(dir string) (*Recorder, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return nil, fmt.Errorf("数据驻留目录为空")
	}
	if err := os.MkdirAll(trimmed, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据驻留目录 %s: %w", trimmed, err)
	}
	return &Recorder{dir: trimmed, now: time.Now}, nil
}

// RequestMeta 是请求侧元信息（写入 request.meta.json）。
type RequestMeta struct {
	ID            string              `json:"id"`
	Time          time.Time           `json:"time"`
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	Protocol      string              `json:"protocol"`
	Model         string              `json:"model"`
	Upstream      string              `json:"upstream"`
	Key           string              `json:"key"`
	Tool          string              `json:"tool,omitempty"`
	ToolVersion   string              `json:"tool_version,omitempty"`
	Session       string              `json:"session"`
	SessionSource string              `json:"session_source"`
	Headers       map[string][]string `json:"headers,omitempty"`
}

// ResponseMeta 是响应侧元信息（写入 response.meta.json）。
type ResponseMeta struct {
	Status          int                 `json:"status,omitempty"`
	Headers         map[string][]string `json:"headers,omitempty"`
	Bytes           int64               `json:"bytes"`
	DurationMS      int64               `json:"duration_ms"`
	RewroteUpstream bool                `json:"upstream_body_rewritten,omitempty"`
	Error           string              `json:"error,omitempty"`
}

// Record 是一个请求的归档句柄。
type Record struct {
	dir       string
	startedAt time.Time
	rewrote   bool
}

// Begin 创建归档目录并写入请求元信息。
func (r *Recorder) Begin(meta RequestMeta) (*Record, error) {
	if meta.ID == "" {
		meta.ID = newRecordID(r.now())
	}
	if meta.Time.IsZero() {
		meta.Time = r.now()
	}
	directory := filepath.Join(r.dir, meta.Time.Format("2006-01-02"), meta.ID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("创建归档目录: %w", err)
	}
	meta.Headers = redactHeaders(meta.Headers)
	if err := writeJSONFile(filepath.Join(directory, "request.meta.json"), meta); err != nil {
		return nil, err
	}
	return &Record{dir: directory, startedAt: meta.Time}, nil
}

// WriteBodies 写入客户端原始请求体与（改写后的）上游请求体。
func (rec *Record) WriteBodies(clientBody, upstreamBody []byte) error {
	if err := writeFile(filepath.Join(rec.dir, "client-request.body"), clientBody); err != nil {
		return err
	}
	rec.rewrote = len(upstreamBody) > 0
	if rec.rewrote {
		return writeFile(filepath.Join(rec.dir, "upstream-request.body"), upstreamBody)
	}
	return nil
}

// ResponseWriter 返回上游响应体的落盘 writer（流式边收边写）。
func (rec *Record) ResponseWriter() (io.WriteCloser, error) {
	return os.OpenFile(filepath.Join(rec.dir, "upstream-response.body"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}

// Finish 写入响应元信息。
func (rec *Record) Finish(meta ResponseMeta) error {
	meta.Headers = redactHeaders(meta.Headers)
	meta.DurationMS = time.Since(rec.startedAt).Milliseconds()
	meta.RewroteUpstream = rec.rewrote
	return writeJSONFile(filepath.Join(rec.dir, "response.meta.json"), meta)
}

// Dir 返回本记录的归档目录（诊断用）。
func (rec *Record) Dir() string { return rec.dir }

// redactedHeaders 是需要脱敏的头名（不落盘）。
var redactedHeaders = []string{
	"authorization", "x-api-key", "api-key", "cookie", "set-cookie",
	"proxy-authorization", "x-opencode-session",
}

func redactHeaders(headers map[string][]string) map[string][]string {
	if headers == nil {
		return nil
	}
	output := make(map[string][]string, len(headers))
	for name, values := range headers {
		lower := strings.ToLower(name)
		redacted := false
		for _, secret := range redactedHeaders {
			if lower == secret {
				redacted = true
				break
			}
		}
		if redacted {
			output[name] = []string{"<redacted>"}
			continue
		}
		output[name] = values
	}
	return output
}

func writeJSONFile(path string, payload any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(encoded, '\n'))
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

func newRecordID(now time.Time) string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return now.Format("150405.000000000")
	}
	return now.Format("150405.000") + "-" + hex.EncodeToString(buffer)
}

// RedactRequestHeaders 把 http.Header 折成可落盘/打日志的脱敏副本。
func RedactRequestHeaders(header http.Header) map[string][]string {
	converted := map[string][]string{}
	for name, values := range header {
		converted[name] = values
	}
	return redactHeaders(converted)
}
