package aigateway

import "encoding/json"

// Body 是延迟解析的请求体：只有后端特化真正写入字段时才解析并标记需重写；
// 未标记时上游收到的是**逐字节原始请求体**（键序、空白、转义都不动）。
type Body struct {
	raw      []byte
	document map[string]any
	mutated  bool
}

// NewBody 包装原始请求体。
func NewBody(raw []byte) *Body { return &Body{raw: raw} }

// Set 写入顶层字段（必要时解析原始 JSON）。
func (b *Body) Set(field string, value any) {
	b.ensureDocument()
	if b.document == nil {
		return
	}
	b.document[field] = value
	b.mutated = true
}

// MetadataSession 按 Anthropic 原生格式写入 metadata.user_id.session_id
// （保留既有 device_id 等键）。
func (b *Body) MetadataSession(sessionID string) {
	if sessionID == "" {
		return
	}
	b.ensureDocument()
	if b.document == nil {
		return
	}
	mergeMetadataSession(b.document, sessionID)
	b.mutated = true
}

// Mutated 报告请求体是否被改写。
func (b *Body) Mutated() bool { return b.mutated }

// Encoded 返回要发给上游的请求体：未改写时是原始字节。
func (b *Body) Encoded() ([]byte, error) {
	if !b.mutated {
		return b.raw, nil
	}
	return json.Marshal(b.document)
}

func (b *Body) ensureDocument() {
	if b.document != nil {
		return
	}
	document := map[string]any{}
	if err := json.Unmarshal(b.raw, &document); err != nil {
		b.document = nil
		return
	}
	b.document = document
}
