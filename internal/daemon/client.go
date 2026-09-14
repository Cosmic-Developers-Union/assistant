package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"assistant/internal/instances"
)

// EndpointEnv / EndpointPathEnv 是自举发现的覆盖点（mcp daemon 与测试用）。
const (
	EndpointEnv     = "ASSISTANT_DAEMON_ENDPOINT"
	EndpointAddrEnv = "ASSISTANT_DAEMON_ADDR"
)

// Client 是 daemon 只读状态 API 的客户端。
type Client struct {
	endpoint Endpoint
	http     *http.Client
}

// Discover 自举发现运行中的 daemon：端点文件 <配置目录>/daemon.json（可用
// ASSISTANT_DAEMON_ENDPOINT 覆盖路径，ASSISTANT_DAEMON_ADDR 覆盖地址）。
// getenv 为 nil 时用 os.Getenv。
func Discover(getenv func(string) string) (*Client, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	path := strings.TrimSpace(getenv(EndpointEnv))
	if path == "" {
		standard, err := instances.DaemonEndpointPath()
		if err != nil {
			return nil, err
		}
		path = standard
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("daemon 未在运行（找不到端点文件 %s）：先 assistant run", path)
		}
		return nil, fmt.Errorf("读取端点文件 %s: %w", path, err)
	}
	var endpoint Endpoint
	if err := json.Unmarshal(data, &endpoint); err != nil {
		return nil, fmt.Errorf("解析端点文件 %s: %w", path, err)
	}
	if addr := strings.TrimSpace(getenv(EndpointAddrEnv)); addr != "" {
		endpoint.Addr = addr
	}
	if endpoint.Addr == "" || endpoint.Token == "" {
		return nil, fmt.Errorf("端点文件 %s 缺少 addr/token（daemon 可能已退出）", path)
	}
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// NewClient 直接指定端点（测试用）。
func NewClient(endpoint Endpoint) *Client {
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: 10 * time.Second}}
}

// Endpoint 返回发现的端点信息。
func (c *Client) Endpoint() Endpoint { return c.endpoint }

// Status 拉取完整状态快照。
func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	err := c.get(ctx, "/api/v1/status", "", &status)
	return status, err
}

// Sessions 拉取进行中的会话。
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var sessions []Session
	err := c.get(ctx, "/api/v1/sessions", "", &sessions)
	return sessions, err
}

// Queue 拉取各仓库最近一轮的待办清单。
func (c *Client) Queue(ctx context.Context) ([]Queue, error) {
	var queue []Queue
	err := c.get(ctx, "/api/v1/queue", "", &queue)
	return queue, err
}

// Results 拉取最近结果（limit<=0 用服务端默认上限）。
func (c *Client) Results(ctx context.Context, limit int) ([]Result, error) {
	query := ""
	if limit > 0 {
		query = fmt.Sprintf("?limit=%d", limit)
	}
	var results []Result
	err := c.get(ctx, "/api/v1/results", query, &results)
	return results, err
}

func (c *Client) get(ctx context.Context, path, query string, out any) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, fmt.Sprintf("http://%s%s%s", c.endpoint.Addr, path, query), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.endpoint.Token)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("请求 daemon %s: %w", c.endpoint.Addr, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("daemon %s 返回 %s: %s", path, response.Status, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("解析 daemon %s 响应: %w", path, err)
	}
	return nil
}
