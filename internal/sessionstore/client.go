package sessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client 是记录库服务端的 HTTP 客户端（assistant serve）。
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient 构造客户端（BaseURL 末尾斜杠会被去掉）。
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// PushResult 是推送结果。
type PushResult struct {
	Stored  int    `json:"stored"`
	Skipped int    `json:"skipped"`
	Message string `json:"message,omitempty"`
}

// Push 上传一批会话记录。
func (c *Client) Push(ctx context.Context, batch Batch) (PushResult, error) {
	var result PushResult
	err := c.do(ctx, http.MethodPost, "/api/v1/sessions", nil, batch, &result)
	return result, err
}

// List 查询会话元数据。
func (c *Client) List(ctx context.Context, filter Filter, limit int) ([]Meta, error) {
	query := filterQuery(filter)
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var payload struct {
		Sessions []Meta `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/sessions", query, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Sessions, nil
}

// Read 读取一条记录的消息。
func (c *Client) Read(ctx context.Context, key Key, offset, limit int) ([]Message, error) {
	query := url.Values{}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var payload struct {
		Messages []Message `json:"messages"`
	}
	path := "/api/v1/sessions/" + url.PathEscape(key.Host) + "/" + url.PathEscape(key.Project) + "/" + url.PathEscape(key.Session)
	if err := c.do(ctx, http.MethodGet, path, query, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Messages, nil
}

// Search 检索记录。
func (c *Client) Search(ctx context.Context, query string, filter Filter, limit int) ([]Match, error) {
	values := filterQuery(filter)
	values.Set("q", query)
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	var payload struct {
		Matches []Match `json:"matches"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/search", values, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Matches, nil
}

// Conversations 返回会话实体及各自的记录条数。
func (c *Client) Conversations(ctx context.Context) (map[string]int, error) {
	var payload struct {
		Conversations map[string]int `json:"conversations"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/conversations", nil, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Conversations, nil
}

func filterQuery(filter Filter) url.Values {
	values := url.Values{}
	if filter.Host != "" {
		values.Set("host", filter.Host)
	}
	if filter.Project != "" {
		values.Set("project", filter.Project)
	}
	if filter.Session != "" {
		values.Set("session", filter.Session)
	}
	if filter.Conversation != "" {
		values.Set("conversation", filter.Conversation)
	}
	if filter.Source != "" {
		values.Set("source", filter.Source)
	}
	if !filter.Since.IsZero() {
		values.Set("since", filter.Since.UTC().Format(time.RFC3339))
	}
	return values
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	if c.BaseURL == "" {
		return fmt.Errorf("没有配置记录库服务端地址（--url / ASSISTANT_SESSIONS_URL / serve.json / sessions-remote.json）")
	}
	endpoint := c.BaseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	if c.Token != "" {
		request.Header.Set("authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("请求 %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s 返回 HTTP %d：%s", endpoint, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("解析 %s 的响应: %w", endpoint, err)
	}
	return nil
}

// RemoteConfig 是记录库服务端的连接配置。
type RemoteConfig struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// RemoteFile 是客户端连接配置的落点（服务端在别的机器上时手写这个文件，0600）。
const RemoteFile = "sessions-remote.json"

// ServeFile 是 assistant serve 在本机写下的端点文件（见 cmd/assistant/serve.go）。
const ServeFile = "serve.json"

// ResolveRemote 解析服务端连接配置：环境变量 > <配置目录>/sessions-remote.json >
// <配置目录>/serve.json（本机 serve 自己写的端点文件）。找不到返回零值（客户端会
// 在调用时给出可读错误）。
func ResolveRemote(configDir string) RemoteConfig {
	config := RemoteConfig{
		URL:   strings.TrimSpace(os.Getenv("ASSISTANT_SESSIONS_URL")),
		Token: strings.TrimSpace(os.Getenv("ASSISTANT_SESSIONS_TOKEN")),
	}
	if config.URL != "" && config.Token != "" {
		return config
	}
	for _, name := range []string{RemoteFile, ServeFile} {
		path := filepath.Join(configDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file RemoteConfig
		if err := json.Unmarshal(data, &file); err != nil {
			continue
		}
		if config.URL == "" {
			config.URL = strings.TrimSpace(file.URL)
		}
		if config.Token == "" {
			config.Token = strings.TrimSpace(file.Token)
		}
		if config.URL != "" && config.Token != "" {
			return config
		}
	}
	return config
}
