package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RepositoryDetails 是仓库的只读元数据（setup 与运行期预检使用）。
type RepositoryDetails struct {
	Repository
	DefaultBranch string
	Private       bool
	Empty         bool
}

// GetRepository 读取单个仓库的元数据。
func (c *Client) GetRepository(ctx context.Context, owner, name string) (RepositoryDetails, error) {
	repository, _, err := c.sdk.Repositories.GetRepo(ctx, owner, name)
	if err != nil {
		return RepositoryDetails{}, fmt.Errorf("get repository %s/%s: %w", owner, name, err)
	}
	return RepositoryDetails{
		Repository:    Repository{Owner: owner, Name: name},
		DefaultBranch: repository.DefaultBranch,
		Private:       repository.Private,
		Empty:         repository.Empty,
	}, nil
}

// CheckHealth 探测 Gitea 是否可达且响应 API（GET /api/v1/version，无需认证）。
// run 启动前对每个 instance 调用，服务不可用时拒绝带病启动。
func (c *Client) CheckHealth(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+"/api/v1/version", nil)
	if err != nil {
		return "", fmt.Errorf("probe gitea version: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("连接 Gitea %s: %w", c.host, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("Gitea %s 健康检查失败 (HTTP %d)", c.host, response.StatusCode)
	}
	version := strings.TrimSpace(string(body))
	var payload struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Version != "" {
		version = payload.Version
	}
	if version == "" {
		version = "unknown"
	}
	return version, nil
}
