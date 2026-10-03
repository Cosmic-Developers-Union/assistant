package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"uuid"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/qq"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/telegram"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/weixin"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// Start 装配配置后运行所有 bot；只读演练不会建运行根、恢复会话或连接消息通道。
func Start(ctx context.Context, cfg *Config, dryRun, verbose bool, log func(string, ...any)) (resultErr error) {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !dryRun {
		release, err := acquireRoot(cfg.Runtime.Root)
		if err != nil {
			return err
		}
		defer release()
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	secrets := []string{cfg.Session.AccessKey, cfg.Session.SecretKey}
	for _, conn := range cfg.Connects {
		secrets = append(secrets, conn.Token, conn.AppSecret)
	}
	for _, server := range cfg.MCP {
		for key, value := range server.Env {
			if secretKey(key) {
				secrets = append(secrets, value)
			}
		}
	}
	defer func() { resultErr = credentials.RedactError(resultErr, secrets...) }()
	originalLog := log
	log = func(format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		for _, secret := range secrets {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "***")
			}
		}
		originalLog("%s", message)
	}
	engine := &Engine{Workers: map[string]Worker{}, Concurrency: cfg.Runtime.Concurrency, Timeout: cfg.Runtime.Timeout, Log: log}
	local := LocalSession{Root: filepath.Join(cfg.Runtime.Root, "sessions")}
	var session Session = local
	if cfg.Session.Store == "s3" {
		remote, err := NewS3Bucket(cfg.Session)
		if err != nil {
			return err
		}
		session = S3Session{Local: local, Remote: remote}
	}
	sources := map[string]*GiteaSource{}
	clients := map[string]*status.Client{}
	type chatTask struct {
		adapter integration.ChatIntegration
		bridge  *chatBridge
	}
	var chats []chatTask
	for _, name := range slices.Sorted(maps.Keys(cfg.Bots)) {
		bot := cfg.Bots[name]
		if cfg.Runtime.Debug {
			log("装配 bot=%s kind=%s workspace=%s session=%s mcp=%v", name, bot.Kind, bot.Workspace.Type, cfg.Session.Store, bot.Agent.MCP)
		}
		var ref string
		for _, value := range bot.Use {
			ref = value
		}
		conn := cfg.Connects[ref]
		spec := bot.Agent
		spec.Prompt = bot.With.Prompt
		spec.Servers = map[string]MCP{}
		if bot.With.Identity != "" {
			spec.Prompt += "\n平台落地身份: " + bot.With.Identity
		}
		for _, serverName := range spec.MCP {
			server := cfg.MCP[serverName]
			server.Env = maps.Clone(server.Env)
			if server.Env == nil {
				server.Env = map[string]string{}
			}
			// 绑定连接只向本连接的 Gitea MCP 注入令牌，不把开发者登录态混入运行。
			if conn.Type == "gitea" && len(server.Args) >= 2 && server.Args[0] == "mcp" && server.Args[1] == "gitea" {
				server.Env["GITEA_HOST"] = conn.URL
				server.Env["GITEA_ACCESS_TOKEN"] = conn.Token
			}
			spec.Servers[serverName] = server
		}
		worker := Worker{Spec: spec, Session: session, Runner: ClaudeRunner{}}
		if verbose {
			worker.Runner = ClaudeRunner{Log: log, Debug: cfg.Runtime.Debug}
		}
		if bot.Kind == "chat" {
			worker.Workspace = DirectoryWorkspace{Root: filepath.Join(cfg.Runtime.Root, "chat", name)}
			if dryRun {
				log("消息 bot %s（%s）：演练不消费消息", name, conn.Type)
			} else {
				adapter, err := chatAdapter(ref, conn, log)
				if err != nil {
					return err
				}
				chats = append(chats, chatTask{adapter: &namedChat{ChatIntegration: adapter, name: ref}, bridge: &chatBridge{engine: engine, bot: name, root: cfg.Runtime.Root}})
			}
		} else {
			worker.Workspace = &WorktreeWorkspace{Root: cfg.Runtime.Root, Connect: conn, Spec: bot.Workspace}
			source := sources[ref]
			if source == nil {
				client, err := status.NewClient(conn.URL, conn.Token)
				if err != nil {
					return err
				}
				if err := client.VerifyAuthentication(ctx); err != nil {
					return err
				}

				source = &GiteaSource{API: client, Host: conn.URL}
				identity, err := client.AuthenticatedUser(ctx)
				if err != nil {
					return err
				}
				source.Identity = identity
				sources[ref] = source
				clients[ref] = client
			}
			if bot.With.Identity != "" {
				user, err := clients[ref].AuthenticatedUser(ctx)
				if err != nil {
					return err
				}
				if user != bot.With.Identity {
					return fmt.Errorf("bot %s 的连接令牌身份与 with.identity 不符", name)
				}
			}
			if bot.Kind == "gitea-review" {
				source.ReviewBot = name
			} else {
				source.TriageBot = name
			}
		}
		engine.Workers[name] = worker
	}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		engine.Sources = append(engine.Sources, sources[name])
	}
	if dryRun {
		return engine.Round(ctx, true)
	}
	observer, err := NewObserver(cfg.Runtime.Root)
	if err != nil {
		return err
	}
	engine.Observe = func(record Record) {
		if err := observer.Save(record); err != nil {
			log("登记观测失败：%v", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if cfg.Runtime.APIListen != "" && cfg.Runtime.APIListen != "off" {
		server, err := observer.Listen(cfg.Runtime.APIListen)
		if err != nil {
			return err
		}
		defer server.Close()
		log("状态 API: %s（凭据位于运行根 api.json）", server.Addr().String())
	}
	failures := make(chan error, len(chats))
	for _, task := range chats {
		wg.Go(func() {
			err := integration.RunChat(ctx, task.adapter, integration.Options{Conversations: task.bridge, Concurrency: cfg.Runtime.Concurrency, Log: log})
			if err != nil && ctx.Err() == nil {
				log("消息 bot %s 已停止：%v", task.bridge.bot, err)
			}
			failures <- err
		})
	}
	log("运行 %d 个 bot，数据根: %s", len(cfg.Bots), cfg.Runtime.Root)
	err = engine.Run(ctx, cfg.Runtime.Interval)
	cancel()
	wg.Wait()
	close(failures)
	var all []error
	all = append(all, err)
	for failure := range failures {
		all = append(all, failure)
	}
	return errors.Join(all...)
}

type namedChat struct {
	integration.ChatIntegration
	name string
}

func (c *namedChat) Name() string { return c.name }
func chatAdapter(name string, conn Connect, log func(string, ...any)) (integration.ChatIntegration, error) {
	switch conn.Type {
	case "qq":
		return qq.NewAdapter(qq.AdapterConfig{AppID: conn.AppID, AppSecret: conn.AppSecret, APIBaseURL: conn.URL, AdminUsers: conn.AdminUsers}, name, false, log), nil
	case "telegram":
		return telegram.NewAdapter(telegram.AdapterConfig{Name: name, BotToken: conn.Token, APIBaseURL: conn.URL, AdminUsers: conn.AdminUsers}, log), nil
	case "weixin":
		return weixin.NewAdapter(weixin.AdapterConfig{Name: name, ClientConfig: weixin.Config{BaseURL: conn.URL, BotToken: conn.Token}, AdminUsers: conn.AdminUsers, LoginUserID: conn.UserID}, log), nil
	default:
		return nil, fmt.Errorf("不支持的消息平台")
	}
}

type chatBridge struct {
	engine    *Engine
	bot, root string
	locks     sync.Map
}

func (b *chatBridge) lock(id string) *sync.Mutex {
	value, _ := b.locks.LoadOrStore(id, new(sync.Mutex))
	return value.(*sync.Mutex)
}
func (b *chatBridge) ConversationFor(transport, user string) (string, error) {
	return transport + "\x00" + user, nil
}
func (b *chatBridge) WorkspaceDir(id string) (string, error) {
	return filepath.Join(b.root, "chat", b.bot, safeHost(id)), nil
}
func (b *chatBridge) Reset(id string) error {
	lock := b.lock(id)
	lock.Lock()
	defer lock.Unlock()
	dir, _ := b.WorkspaceDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".generation"), []byte(uuid.New().String()), 0o600)
}
func (b *chatBridge) Handle(ctx context.Context, id string, turn integration.Turn) (string, error) {
	lock := b.lock(id)
	lock.Lock()
	defer lock.Unlock()
	dir, _ := b.WorkspaceDir(id)
	generation, err := os.ReadFile(filepath.Join(dir, ".generation"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	host, user, _ := strings.Cut(id, "\x00")
	ev := Event{Bot: b.bot, Kind: "chat", Host: host, User: user, Title: string(generation), Text: turn.Text}
	// 复用同一生命周期；局部观测同时取得回复，观测不驱动调度。
	copyEngine := &Engine{Workers: b.engine.Workers, Timeout: b.engine.Timeout, Observe: func(record Record) {
		if b.engine.Observe != nil {
			b.engine.Observe(record)
		}
	}}
	var result string
	copyEngine.Observe = func(record Record) {
		result = record.Outcome.Result
		if b.engine.Observe != nil {
			b.engine.Observe(record)
		}
	}
	err = copyEngine.Process(ctx, ev)
	return result, err
}
