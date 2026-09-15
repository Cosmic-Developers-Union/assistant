// ai-gateway 是 assistant 套件的自托管 API 网关：对客户端暴露 Anthropic
// Messages 协议，向上游 Anthropic 兼容厂商（zhipu / kimi / minimax /
// opencode / openai 代理 / Anthropic 官方）转发，并按上游规则做故障转移。
//
// 核心差异化见 internal/aigateway：跨客户端的会话识别（显式头 → Claude Code
// 的 metadata.user_id → 内容确定性派生）与厂商化注入（x-opencode-session、
// x-session-affinity、prompt_cache_key 等），通用网关不具备。
//
// 用法：
//
//	ai-gateway --config <配置目录>/ai-gateway.json
//	ai-gateway --listen 127.0.0.1:8780   # 覆盖监听地址
//
// 配置示例见仓库 ai-gateway.example.json；assistant 侧只需一个 provider 指向
// 本网关（api_key = keys[].token，env.ANTHROPIC_BASE_URL = 网关地址）。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"assistant/internal/aigateway"
	"assistant/internal/instances"
)

// version 由构建注入（-X main.version=...）；缺省 dev。
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	var configPath string
	var listen string
	flag.StringVar(&configPath, "config", "", "配置文件路径（缺省 <配置目录>/ai-gateway.json）")
	flag.StringVar(&listen, "listen", "", "监听地址（覆盖配置中的 listen）")
	flag.Parse()

	path := configPath
	if path == "" {
		directory, err := instances.DefaultConfigDir()
		if err != nil {
			log.Fatalf("定位配置目录: %v", err)
		}
		path = filepath.Join(directory, "ai-gateway.json")
	}
	config, err := aigateway.Load(path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if listen != "" {
		config.Listen = listen
	}
	if config.Residency.Enabled && config.Residency.Dir == "" {
		// 缺省归档到配置目录下，避免相对路径随工作目录漂移
		config.Residency.Dir = filepath.Join(filepath.Dir(path), "ai-gateway-residency")
	}
	if config.UserAgent == aigateway.DefaultUserAgent {
		config.UserAgent = "assistant-ai-gateway/" + version
	}

	gateway, err := aigateway.New(config, func(format string, args ...any) {
		log.Printf(format, args...)
	})
	if err != nil {
		log.Fatalf("%v", err)
	}
	if config.Residency.Enabled {
		log.Printf("数据驻留已启用：%s", config.Residency.Dir)
	}
	server := &http.Server{
		Addr:              config.Listen,
		Handler:           gateway.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("ai-gateway 监听 %s（上游 %d 个，路由 %d 条）", config.Listen, len(config.Upstreams), len(config.Routes))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("网关退出: %v", err)
		}
	}()
	<-ctx.Done()
	log.Printf("收到退出信号，等待在途请求结束（10s）")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	log.Printf("已退出")
}
