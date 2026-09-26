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

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"

	"github.com/spf13/cobra"
)

// serveOptions 是 `assistant serve` 的参数。
type serveOptions struct {
	Listen string
	Root   string
	Host   string
	Token  string
	Force  bool
	Quiet  bool

	// Rand 是随机令牌的来源，默认 crypto/rand.Read（见 randomToken）。抽出来
	// 是为了让「取随机数失败」这条启动路径可测：它决定 serve 能不能生成访问
	// 令牌，而随机源在真机上几乎不可能失手，只有注入才能走到那个错误分支。
	// 生产路径不设置它。
	Rand func([]byte) (int, error)

	// Getwd 是缺省记录库目录的定位来源，默认 os.Getwd（见 defaultSessionsRoot）。
	// 抽出来是为了让「当前目录取不出来」这条路径可测：此时 serve 继续跑会把
	// 记录库落到一个未知位置，必须提前失败。生产路径不设置它。
	Getwd func() (string, error)
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
	flags.StringVar(&options.Root, "root", "",
		"记录库目录（缺省 <数据目录>/Cosmic-Developers-Union/assistant/sessions；"+
			"目录名不参与身份判断——库根必须有 manifest.json，非空且无 manifest 的目录会被拒绝）")
	flags.StringVar(&options.Host, "host", "", "宿主机标签（写进 manifest，便于人工分辨是哪台机器的库；缺省主机名）")
	flags.BoolVar(&options.Force, "force", false, "接管一个非空但没有 manifest 的目录（确认它不是别人的数据再开）")
	flags.StringVar(&options.Token, "token", "", "访问令牌（缺省随机生成并写入 serve.json）")
	flags.BoolVar(&options.Quiet, "quiet", false, "不打印每条请求日志")
	return command
}

func runServe(command *cobra.Command, configPath string, options *serveOptions) error {
	root := strings.TrimSpace(options.Root)
	if root == "" {
		defaultRoot, err := defaultSessionsRoot(options.Getwd)
		if err != nil {
			return err
		}
		root = defaultRoot
	}
	store, err := sessionstore.Open(root, sessionstore.OpenOptions{Host: options.Host, Force: options.Force})
	if err != nil {
		return err
	}
	token := strings.TrimSpace(options.Token)
	if token == "" {
		token, err = randomToken(options.Rand)
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
	// 监听成功后回填真实地址：`:0` 之类的写法只有 listener 知道最终端口，
	// 测试与将来的诊断输出都需要它。
	serveListenAddress = listener.Addr().String()
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
	manifest := store.Manifest()
	fmt.Fprintf(stdout, "记录库：%s（格式 %s v%d，host=%s，布局 %s）\n",
		store.Root(), manifest.Format, manifest.Version, manifest.Host, manifest.Layout)
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

// serveListenAddress 记录最近一次成功监听的地址（由 runServe 写、供测试读取）：
// 监听地址写成 ":0" 时，最终端口只有 OS 知道，测试要连上去必须拿到真实地址。
// 生产逻辑不依赖它，它只是给测试留的一个观测点。
var serveListenAddress string

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
	serveEndpointWritten()
	return path, nil
}

// serveEndpointWritten 在端点文件写成功后调用，默认是空操作。
//
// 这是一个**时序钩子**，不是依赖注入：它给出「文件已落盘」的确定时点，让测试能在
// 那一刻取消 ctx，钉住「写完但没起来就退出」时端点文件必须被清掉（否则同机客户端
// 会连到一个不存在的端口，报出来的是「连接被拒绝」，与真实原因无关）。这类时序写
// 成 sleep 就会在慢机器上假失败。生产路径不设置它。
var serveEndpointWritten = func() {}

func serveHandler(store *sessionstore.Store, token string, logf func(string, ...any)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		manifest := store.Manifest()
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"root":    store.Root(),
			"format":  manifest.Format,
			"version": manifest.Version,
			"host":    manifest.Host,
		})
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
			handleSessionPush(writer, request, store, logf)
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

// handleSessionPush 处理 POST /api/v1/sessions：把一批记录写进记录库。
//
// 与 GET 分开是为了让「写入中途失败」这条分支能被单独走到——库根被写坏时
// store.Put 会报错，而列表/读取路径不经过它。响应里必须带上已经存下的条数：
// 客户端据此知道要不要重推整批，退回一个没有 stored 的 400 会让它重复写入。
func handleSessionPush(writer http.ResponseWriter, request *http.Request, store *sessionstore.Store, logf func(string, ...any)) {
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

// writeJSON 是所有接口的唯一响应出口。先设 content-type 与状态码再编码：编码
// 万一失手，客户端仍拿得到状态码，而不是一个没有头的半截响应（那会把排查引向
// 网络问题而不是服务端的值）。
func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

// randomToken 生成访问令牌；randSource 为 nil 时用 crypto/rand.Read。
func randomToken(randSource func([]byte) (int, error)) (string, error) {
	if randSource == nil {
		randSource = rand.Read
	}
	buffer := make([]byte, 16)
	if _, err := randSource(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// defaultSessionsRoot 返回记录库缺省目录：当前目录的 data/sessions
// （显式模式：一切产物收在配置旁边）。getwd 为 nil 时用 os.Getwd。
func defaultSessionsRoot(getwd func() (string, error)) (string, error) {
	if getwd == nil {
		getwd = os.Getwd
	}
	directory, err := getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "data", "sessions"), nil
}

// resolveConfigDir 返回配置目录（--config/ASSISTANT_CONFIG 优先，否则平台标准位置）。
func resolveConfigDir(configPath string) (string, error) {
	path, err := configTargetPath(configPath)
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}
