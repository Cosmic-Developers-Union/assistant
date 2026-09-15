package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"assistant/internal/sessionstore"

	"github.com/spf13/cobra"
)

// serveOptions 是 `assistant serve` 的参数。
type serveOptions struct {
	Listen string
	Root   string
	Token  string
	Quiet  bool
}

// newServeCommand 是会话记录服务端：接受 `assistant session push` 上传的记录，
// 按 (host, project, session) 存储，并提供列表/读取/检索接口（agent 的 sessions MCP
// 也走这些接口）。本机启用时把端点与令牌写到 <配置目录>/serve.json 供自举。
func newServeCommand(configFlag *string) *cobra.Command {
	options := &serveOptions{}
	command := &cobra.Command{
		Use:   "serve",
		Short: "会话记录服务端：接收 session push、按 host/project/session 存储并提供检索",
		Long: "为 `assistant session push` 与 sessions MCP（session_search/session_list/\n" +
			"session_read/conversation_list）提供服务：\n" +
			"  - 记录按 (host, project, session) 存成原样 jsonl，另存一份 meta.json 供检索；\n" +
			"  - 聊天会话按会话实体（conversation）归类，跨通道可查；\n" +
			"  - 本机启动时把 URL 与令牌写到 <配置目录>/serve.json（0600，退出即删），\n" +
			"    同机客户端与 agent 的 MCP 自动发现；远端部署请给客户端写 sessions-remote.json。\n" +
			"接口：GET /healthz、POST /api/v1/sessions、GET /api/v1/sessions[/host/project/session]、\n" +
			"GET /api/v1/search、GET /api/v1/conversations（除 healthz 外都要 Bearer 令牌）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runServe(command, *configFlag, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Listen, "listen", "127.0.0.1:8780", "监听地址")
	flags.StringVar(&options.Root, "root", "", "记录库目录（缺省 <数据目录>/Cosmic-Developers-Union/assistant/sessions）")
	flags.StringVar(&options.Token, "token", "", "访问令牌（缺省随机生成并写入 serve.json）")
	flags.BoolVar(&options.Quiet, "quiet", false, "不打印每条请求日志")
	return command
}

func runServe(command *cobra.Command, configPath string, options *serveOptions) error {
	root := strings.TrimSpace(options.Root)
	if root == "" {
		defaultRoot, err := defaultSessionsRoot()
		if err != nil {
			return err
		}
		root = defaultRoot
	}
	store, err := sessionstore.NewStore(root)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(options.Token)
	if token == "" {
		token, err = randomToken()
		if err != nil {
			return err
		}
	}
	configDir, err := resolveConfigDir(configPath)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", options.Listen, err)
	}
	address := options.Listen
	if strings.HasPrefix(options.Listen, ":") {
		address = "127.0.0.1" + options.Listen
	}

	stdout := command.OutOrStdout()
	logf := func(format string, arguments ...any) {
		if options.Quiet {
			return
		}
		fmt.Fprintf(stdout, "[serve %s] %s\n",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fmt.Sprintf(format, arguments...))
	}
	handler := serveHandler(store, token, logf)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second}

	endpointPath, err := writeServeEndpoint(configDir, serveEndpoint{
		URL:       "http://" + address,
		Token:     token,
		Root:      root,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	defer os.Remove(endpointPath)
	fmt.Fprintf(stdout, "记录库：%s\n", root)
	fmt.Fprintf(stdout, "监听：  http://%s\n", address)
	fmt.Fprintf(stdout, "端点凭据：%s（0600，退出即删）\n", endpointPath)
	fmt.Fprintf(stdout, "客户端：assistant session push（同机自动发现）；远端见 sessions-remote.json\n")

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-command.Context().Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		fmt.Fprintln(stdout, "已停止监听")
		return nil
	}
}

// serveEndpoint 是 serve.json 的内容（本机客户端与 agent MCP 自举读取）。
type serveEndpoint struct {
	URL       string `json:"url"`
	Token     string `json:"token,omitempty"`
	Root      string `json:"root,omitempty"`
	PID       int    `json:"pid,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

func writeServeEndpoint(configDir string, endpoint serveEndpoint) (string, error) {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(configDir, sessionstore.ServeFile)
	encoded, err := json.MarshalIndent(endpoint, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func serveHandler(store *sessionstore.Store, token string, logf func(string, ...any)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "sessions": store.Root()})
	})
	authorize := func(writer http.ResponseWriter, request *http.Request) bool {
		provided := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
			return true
		}
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "令牌无效"})
		return false
	}
	mux.HandleFunc("/api/v1/sessions", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		switch request.Method {
		case http.MethodPost:
			var batch sessionstore.Batch
			if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "解析请求体失败：" + err.Error()})
				return
			}
			stored := 0
			for _, session := range batch.Sessions {
				meta, err := store.Put(session)
				if err != nil {
					writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error(), "stored": stored})
					return
				}
				stored++
				logf("已存 %s（%d 行，来源 %s）", meta.Key.String(), meta.Lines, meta.Source)
			}
			writeJSON(writer, http.StatusOK, map[string]any{"stored": stored})
		case http.MethodGet:
			metas, err := store.List(listFilter(request), intQuery(request, "limit", 0))
			if err != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if metas == nil {
				metas = []sessionstore.Meta{}
			}
			writeJSON(writer, http.StatusOK, map[string]any{"sessions": metas})
		default:
			writeJSON(writer, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
		}
	})
	mux.HandleFunc("/api/v1/search", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		matches, err := store.Search(request.URL.Query().Get("q"), listFilter(request), intQuery(request, "limit", 50))
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if matches == nil {
			matches = []sessionstore.Match{}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"matches": matches})
	})
	mux.HandleFunc("/api/v1/conversations", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		counts, err := store.Conversations()
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"conversations": counts})
	})
	mux.HandleFunc("/api/v1/sessions/", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/sessions/"), "/")
		if len(parts) != 3 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "路径形如 /api/v1/sessions/<host>/<project>/<session>"})
			return
		}
		key := sessionstore.Key{Host: parts[0], Project: parts[1], Session: parts[2]}
		messages, err := store.Read(key, intQuery(request, "offset", 0), intQuery(request, "limit", 0))
		if err != nil {
			writeJSON(writer, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		if messages == nil {
			messages = []sessionstore.Message{}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"key": key, "messages": messages})
	})
	return mux
}

func listFilter(request *http.Request) sessionstore.Filter {
	filter := sessionstore.Filter{
		Host:         strings.TrimSpace(request.URL.Query().Get("host")),
		Project:      strings.TrimSpace(request.URL.Query().Get("project")),
		Session:      strings.TrimSpace(request.URL.Query().Get("session")),
		Conversation: strings.TrimSpace(request.URL.Query().Get("conversation")),
		Source:       strings.TrimSpace(request.URL.Query().Get("source")),
	}
	if since := strings.TrimSpace(request.URL.Query().Get("since")); since != "" {
		if parsed, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = parsed
		}
	}
	return filter
}

func intQuery(request *http.Request, name string, fallback int) int {
	value := strings.TrimSpace(request.URL.Query().Get(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func randomToken() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// defaultSessionsRoot 返回记录库缺省目录：<数据目录>/Cosmic-Developers-Union/assistant/sessions。
func defaultSessionsRoot() (string, error) {
	dataDir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "Cosmic-Developers-Union", "assistant", "sessions"), nil
}

// resolveConfigDir 返回配置目录（--config/ASSISTANT_CONFIG 优先，否则平台标准位置）。
func resolveConfigDir(configPath string) (string, error) {
	path, err := configTargetPath(configPath)
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}
