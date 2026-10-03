package runtime

import (
	"bufio"
	"context"
	"crypto/subtle"
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"
)

// Observer 保存执行元数据，不复制 Claude 的消息记忆，不参与入队判定。
type Observer struct {
	Root string
	mu   sync.Mutex
}

// NewObserver 将观测数据放在运行根，目录在首次保存时创建。
func NewObserver(root string) (*Observer, error) {
	if root == "" {
		return nil, fmt.Errorf("观测根目录不能为空")
	}
	return &Observer{Root: root}, nil
}

// Save 每次执行存一条元数据，清除回复、原始错误流与用户文本。
func (o *Observer) Save(record Record) error {
	record.Outcome.Result = ""
	record.Outcome.Errors = nil
	record.Outcome.APIError = ""
	record.Outcome.MCPStatus = nil
	record.Event.Text = ""
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return atomicFile(filepath.Join(o.Root, "observations", safeHost(record.Session)+"-"+uuid.New().String()+".json"), data)
}

// Records 只读列出最近的执行元数据；损坏记录报错，不伪造空历史。
func (o *Observer) Records() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(o.Root, "observations"))
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []Record{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(o.Root, "observations", entry.Name()))
		if err != nil {
			return nil, err
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("读取观测记录: %w", err)
		}
		result = append(result, record)
	}
	slices.SortFunc(result, func(a, b Record) int { return b.Started.Compare(a.Started) })
	return result, nil
}

func atomicFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// StateServer 拥有状态监听器和描述文件，退出时一并收尾。
type StateServer struct {
	net.Listener
	server *http.Server
	path   string
}

// Close 停止监听并删除临时访问凭据。
func (s *StateServer) Close() error { return errors.Join(s.server.Close(), os.Remove(s.path)) }

// Listen 提供 Bearer 鉴权的只读状态 API，不把控制面变成调度输入。
func (o *Observer) Listen(address string) (*StateServer, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	token := uuid.New().String() + uuid.New().String()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
			http.Error(w, "认证失败", http.StatusUnauthorized)
			return
		}
		records, err := o.Records()
		if err != nil {
			http.Error(w, "无法读取观测数据", http.StatusInternalServerError)
			return
		}
		data, err := json.Marshal(records)
		if err != nil {
			http.Error(w, "无法编码观测数据", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	descriptor, err := json.Marshal(map[string]string{"address": listener.Addr().String(), "token": token})
	if err != nil {
		listener.Close()
		return nil, err
	}
	path := filepath.Join(o.Root, "api.json")
	if err := atomicFile(path, descriptor); err != nil {
		listener.Close()
		return nil, err
	}
	go func() { _ = server.Serve(listener) }()
	return &StateServer{Listener: listener, server: server, path: path}, nil
}

// ServeSessions 通过 stdio MCP 查询本地元数据；工具不会读取或修改平台。
func ServeSessions(ctx context.Context, path string, input io.Reader, output io.Writer, version string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	observer, err := NewObserver(cfg.Runtime.Root)
	if err != nil {
		return err
	}
	return serveSessions(ctx, observer, input, output, version)
}

func serveSessions(ctx context.Context, observer *Observer, input io.Reader, output io.Writer, version string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if closer, ok := input.(io.ReadCloser); ok {
		defer closer.Close()
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	type scanResult struct {
		data []byte
		err  error
		done bool
	}
	lines := make(chan scanResult)
	go func() {
		for scanner.Scan() {
			line := scanResult{data: append([]byte(nil), scanner.Bytes()...)}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		select {
		case lines <- scanResult{done: true, err: scanner.Err()}:
		case <-ctx.Done():
		}
	}()
	for {
		var line scanResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line = <-lines:
		}
		if line.done {
			return line.err
		}
		var request struct {
			ID     jsonv1.RawMessage `json:"id"`
			Method string            `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Session string `json:"session"`
					Limit   int    `json:"limit"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line.data, &request); err != nil {
			return fmt.Errorf("解析 MCP 请求: %w", err)
		}
		if len(request.ID) == 0 {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "assistant-sessions", "version": version}}
		case "ping":
			response["result"] = map[string]any{}
		case "tools/list":
			tools := []map[string]any{}
			for _, name := range []string{"session_list", "session_read"} {
				tools = append(tools, map[string]any{"name": name, "description": "查询会话执行元数据", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"session": map[string]string{"type": "string"}, "limit": map[string]string{"type": "integer"}}}})
			}
			response["result"] = map[string]any{"tools": tools}
		case "tools/call":
			records, err := observer.Records()
			if request.Params.Name != "session_list" && request.Params.Name != "session_read" {
				err = fmt.Errorf("未知查询工具")
			}
			if request.Params.Name == "session_read" && request.Params.Arguments.Session == "" {
				err = fmt.Errorf("session_read 缺少 session")
			}
			if err != nil {
				response["result"] = map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": err.Error()}}}
				break
			}
			selected := []Record{}
			limit := request.Params.Arguments.Limit
			if limit <= 0 {
				limit = 50
			}
			limit = min(limit, 500)
			for _, record := range records {
				if request.Params.Arguments.Session != "" && record.Session != request.Params.Arguments.Session {
					continue
				}
				selected = append(selected, record)
				if len(selected) >= limit {
					break
				}
			}
			data, err := json.Marshal(selected)
			if err != nil {
				return err
			}
			response["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": string(data)}}}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "未知方法"}
		}
		data, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "%s\n", data); err != nil {
			return err
		}
	}
}
