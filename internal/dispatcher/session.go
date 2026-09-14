package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SessionID 按待办派生稳定的 UUID（基于 SHA-256 的 RFC 4122 布局）：同一站点 +
// 仓库 + 待办 + 锚点（PR 的 head / Issue 的标题）重试时复用同一会话记录，
// RunSession 见到已存在的文本记录会以 --resume 续接；head 变化即换新 ID。
func SessionID(host, repository, kind string, number int64, anchor string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"assistant-session", host, repository, kind, fmt.Sprintf("%d", number), anchor,
	}, "|")))
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50 // 版本 5（名字派生）
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // RFC 4122 变体
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

// SessionTitle 生成会话显示名（--name）：便于在会话选择器与
// `claude --resume <title>` 中识别；同一待办的重试标题稳定。
func SessionTitle(kind, repository string, number int64, head string) string {
	switch kind {
	case KindPull:
		base := fmt.Sprintf("review %s#%d", repository, number)
		if short := shortHead(head); short != "" {
			return base + "@" + short
		}
		return base
	default:
		return fmt.Sprintf("triage %s#%d", repository, number)
	}
}

func shortHead(head string) string {
	trimmed := strings.TrimSpace(head)
	if len(trimmed) < 7 {
		return ""
	}
	return trimmed[:7]
}
