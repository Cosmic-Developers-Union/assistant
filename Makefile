# Makefile for assistant

BINARY_NAME = assistant

# Go 构建参数
CGO_ENABLED = 0
GOOS = linux
GOARCH = amd64
LDFLAGS = -w -s

# 手动发布的 package registry 命名空间（generic package 归属于 owner）
GITEA_OWNER ?= owner

# 容器镜像名（供 CI / 仓库级 Actions 使用）
IMAGE ?= assistant:dev

# 评审会话镜像（assistant run --docker-image 使用）
REVIEW_IMAGE ?= ghcr.io/cosmic-developers-union/assistant-review:dev

# daemon 镜像（docker-compose.yaml：调度 + 状态 API + 微信桥常驻容器）
DAEMON_IMAGE ?= ghcr.io/cosmic-developers-union/assistant-daemon:latest

# 安装前缀（make install；DESTDIR 支持打包场景）
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
INSTALL_DIR := $(DESTDIR)$(BINDIR)

# 安装目标目录当前是否可写（不存在时向上找最近的存在目录）
INSTALL_WRITABLE := $(shell \
	if [ -e "$(INSTALL_DIR)" ]; then \
		[ -w "$(INSTALL_DIR)" ] && echo yes; \
	else \
		d="$(INSTALL_DIR)"; \
		while [ ! -e "$$d" ] && [ "$$d" != "/" ]; do d=$$(dirname "$$d"); done; \
		[ -w "$$d" ] && echo yes; \
	fi)

# 非 root 且目标不可写时自动 sudo（已 root 或可写目录如 PREFIX=$HOME/.local 则不 sudo）
SUDO := $(shell [ "$$(id -u)" != "0" ] && [ "$(INSTALL_WRITABLE)" != "yes" ] && echo sudo)

.PHONY: help
help: ## 显示帮助信息
	@echo "assistant Makefile"
	@echo ""
	@echo "用法: make [target]"
	@echo ""
	@echo "可用目标:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'
	@echo ""
	@echo "示例:"
	@echo "  make build               # 构建 assistant 二进制 (Linux/amd64)"
	@echo "  make build-local         # 构建本地平台二进制"
	@echo "  make install             # 先构建再安装到本机 (PREFIX 可改，缺省 /usr/local)"
	@echo "  make test                # 运行测试"
	@echo "  make push                # 手动发布: 构建并推送 latest 到 generic package registry"
	@echo ""
	@echo "正式发布无需手动操作: 合入 main 后 CI 自动发布时间戳版本并移动 latest 指针"
	@echo "make push 用于引导或紧急修复, 需要 GITEA_HOST / GITEA_ACCESS_TOKEN(环境变量或 .env)"

.PHONY: build
build: ## 构建 assistant 二进制 (Linux/amd64, 静态链接, 供 generic package 发布)
	@echo "==> 构建 $(BINARY_NAME) 二进制..."
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -ldflags="$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/assistant
	@echo "==> 构建完成: $(BINARY_NAME)"

.PHONY: build-local
build-local: ## 构建本地平台二进制 (用于开发测试)
	@echo "==> 构建本地平台二进制..."
	go build -o $(BINARY_NAME) ./cmd/assistant
	@echo "==> 构建完成: $(BINARY_NAME)"

.PHONY: install
install: build-local ## 先构建再安装到本机 (缺省 /usr/local；非 root 自动 sudo，PREFIX=$HOME/.local 可免)
	@echo "==> 安装 $(BINARY_NAME) 到 $(INSTALL_DIR)...$(if $(SUDO),（需要 sudo）)"
	$(SUDO) install -d "$(INSTALL_DIR)"
	$(SUDO) install -m 0755 $(BINARY_NAME) "$(INSTALL_DIR)/$(BINARY_NAME)"
	@echo "==> 已安装: $(INSTALL_DIR)/$(BINARY_NAME)"
	@echo "    shell 补全: source <($(BINARY_NAME) completion bash)（或写入系统补全目录）"

.PHONY: test
test: ## 运行测试
	@echo "==> 运行测试..."
	go test -v ./...

.PHONY: image
image: ## 构建容器镜像（仓库级 Actions 用；推送到 registry 由 CI 完成）
	@echo "==> 构建容器镜像 $(IMAGE)..."
	docker build --build-arg VERSION="$$(git describe --tags --always 2>/dev/null || echo dev)" -t $(IMAGE) .
	@echo "==> 构建完成: $(IMAGE)"

.PHONY: review-image
review-image: ## 构建评审会话镜像（target review；Dockerfile 在 images/review/）
	@echo "==> 构建评审会话镜像 $(REVIEW_IMAGE)..."
	docker build -f images/review/Dockerfile --target review -t $(REVIEW_IMAGE) .
	@echo "==> 构建完成: $(REVIEW_IMAGE)"

.PHONY: daemon-image
daemon-image: ## 构建 daemon 镜像（评审环境 + assistant 二进制；docker compose 使用）
	@echo "==> 构建 daemon 镜像 $(DAEMON_IMAGE)..."
	docker build -f images/review/Dockerfile --target daemon \
		--build-arg VERSION="$$(git describe --tags --always 2>/dev/null || echo dev)" \
		-t $(DAEMON_IMAGE) .
	@echo "==> 构建完成: $(DAEMON_IMAGE)"

.PHONY: compose-up
compose-up: ## 预建挂载点并启动 docker compose 部署（daemon；等价 compose up -d）
	@echo "==> 预建挂载点..."
	@mkdir -p "$(HOME)/.claude" "$(HOME)/.config/Cosmic-Developers-Union/assistant" \
		"$(HOME)/.local/share/Cosmic-Developers-Union/assistant"
	@echo "==> 启动 docker compose..."
	docker compose up -d

.PHONY: test-e2e
test-e2e: ## 起临时 Gitea（docker compose）并运行端到端测试
	@echo "==> 启动临时 Gitea..."
	@./test/gitea/up.sh
	@set +e; \
	echo "==> 运行 e2e 测试..."; \
	go test -tags e2e -count=1 -v ./test/e2e/...; \
	status=$$?; \
	./test/gitea/down.sh; \
	exit $$status

.PHONY: gitea-up
gitea-up: ## 只启动临时 Gitea（保留现场，供手动调试）
	@./test/gitea/up.sh

.PHONY: gitea-down
gitea-down: ## 停止并清除临时 Gitea（含数据卷）
	@./test/gitea/down.sh

.PHONY: push
push: build ## 手动发布: 构建并推送 latest 到 generic package registry（引导/紧急修复用；正式发布由 CI 在合入 main 时自动完成）
	@set -e; \
	if [ -z "$$GITEA_HOST$$GITEA_ACCESS_TOKEN" ] && [ -f .env ]; then \
		set -a; . ./.env; set +a; \
	fi; \
	: "$${GITEA_HOST:?需要 GITEA_HOST(环境变量或 .env)}"; \
	: "$${GITEA_ACCESS_TOKEN:?需要 GITEA_ACCESS_TOKEN(环境变量或 .env)}"; \
	BASE="$$GITEA_HOST/api/packages/$(GITEA_OWNER)/generic/$(BINARY_NAME)"; \
	echo "==> 删除旧 latest 指针(不存在则忽略)"; \
	curl -sS -X DELETE -H "Authorization: token $$GITEA_ACCESS_TOKEN" -o /dev/null "$$BASE/latest" || true; \
	echo "==> 上传 $(BINARY_NAME) 到 $$BASE/latest/"; \
	curl -fSL -H "Authorization: token $$GITEA_ACCESS_TOKEN" --upload-file $(BINARY_NAME) -o /dev/null \
		"$$BASE/latest/$(BINARY_NAME)"; \
	echo "==> 已发布 latest($(GITEA_OWNER)/$(BINARY_NAME))"

.PHONY: clean
clean: ## 清理构建产物
	@echo "==> 清理构建产物..."
	@rm -f $(BINARY_NAME) $(BINARY_NAME).exe
	@echo "==> 清理完成"

.DEFAULT_GOAL := help
