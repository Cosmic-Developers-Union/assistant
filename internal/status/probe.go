package status

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeGitea 判断 host 是否是 Gitea/Forgejo 站点：GET /api/v1/version 返回
// 200 且带 version 字段。GitHub 等同名端点为 404，据此区分多个 remote。
func ProbeGitea(ctx context.Context, host string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimRight(host, "/")+"/api/v1/version", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return false
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Version != ""
}
