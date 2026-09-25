package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/statestore"
)

// Endpoint 是 daemon 的 API 端点凭据，写入 <配置目录>/daemon.json 供 MCP 自举发现。
type Endpoint struct {
	Addr      string    `json:"addr"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// Serve 在 listen 上提供只读状态 API（Bearer token 鉴权），返回端点并落盘
// daemon.json（0600，落 configPath 同目录；ctx 取消时关闭服务并删除端点文件，
// 进程崩溃会残留——下次启动覆盖同路径）。listen 支持 ":0"（随机端口，实际地址
// 以返回值/端点文件为准）。configPath 是解析后的 config.json 路径，决定端点
// 文件落点；空串回落 ASSISTANT_CONFIG/平台配置目录。
func Serve(ctx context.Context, listen, configPath string, store *Store, version string, logf func(string, ...any), state ...*statestore.Store) (*Endpoint, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", listen, err)
	}
	token, err := randomToken()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	endpoint := &Endpoint{
		Addr:      listener.Addr().String(),
		Token:     token,
		PID:       os.Getpid(),
		Version:   version,
		StartedAt: time.Now(),
	}

	path, err := instances.DaemonEndpointPathFor(configPath)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := writeEndpoint(path, endpoint); err != nil {
		_ = listener.Close()
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "version": version})
	})
	mux.Handle("GET /api/v1/status", auth(token, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, store.Snapshot())
	}))
	mux.Handle("GET /api/v1/sessions", auth(token, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, store.Snapshot().Sessions)
	}))
	mux.Handle("GET /api/v1/queue", auth(token, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, store.Snapshot().Queue)
	}))
	mux.Handle("GET /api/v1/results", auth(token, func(writer http.ResponseWriter, request *http.Request) {
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		recent := store.Snapshot().Recent
		if limit > 0 && limit < len(recent) {
			recent = recent[:limit]
		}
		writeJSON(writer, http.StatusOK, recent)
	}))
	// SQLite 状态库内省（run.yaml 配了 state-dir/state-file 时可用）：返回持久
	// 化的全量状态——daemon 重启后内存快照清零，这里仍能看到历史会话与结果
	var stateStore *statestore.Store
	for _, candidate := range state {
		if candidate != nil {
			stateStore = candidate
			break
		}
	}
	if stateStore != nil {
		mux.Handle("GET /api/v1/state", auth(token, func(writer http.ResponseWriter, request *http.Request) {
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			if limit <= 0 {
				limit = 50
			}
			snapshot, err := stateStore.Snapshot(limit)
			if err != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(writer, http.StatusOK, snapshot)
		}))
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("状态 API 退出：%v", err)
		}
		_ = os.Remove(path)
	}()
	logf("状态 API 监听 %s（端点凭据 %s，0600）", endpoint.Addr, path)
	return endpoint, nil
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成 API 令牌: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func writeEndpoint(path string, endpoint *Endpoint) error {
	data, err := json.MarshalIndent(endpoint, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// auth 校验 Bearer token（常量时间比较）。
func auth(token string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
			return
		}
		next(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
