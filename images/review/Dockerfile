# syntax=docker/dockerfile:1
# 评审/daemon 共用镜像定义（多 target）：
#   --target review   评审会话镜像：dispatcher 以 `docker run <image> claude ...`
#                     在其中跑 headless 评审（见 `assistant run --docker-image`）。
#                     容器内外路径一致（worktree 与 .mcp.json 按相同绝对路径挂载），
#                     因此不需要 ENTRYPOINT。
#   --target daemon   daemon 镜像：评审环境 + assistant 自身二进制，供
#                     docker-compose.yaml 常驻 `assistant run`（调度/状态 API/微信桥）。
# 两个 target 共享同一 review-env 层，避免环境定义分叉。
ARG BASE_IMAGE=docker.gitea.com/runner-images:ubuntu-24.04

FROM ${BASE_IMAGE} AS review-env
LABEL authors="ge"

USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# 基础工具与依赖（低频变化，单独层便于缓存）
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       bash ca-certificates curl wget git git-lfs openssh-client \
       zip unzip xz-utils zstd jq ripgrep less file gettext-base \
       python3 python3-pip python3-venv python-is-python3 pipx \
       build-essential cmake ninja-build pkg-config libssl-dev \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Bun（skills CLI 与前端工具链）
RUN set -eu; \
    curl --fail --show-error --location --retry 5 https://bun.sh/install | bash; \
    /root/.bun/bin/bun --version

# Claude Code CLI：npm 优先，失败回退官方脚本（低带宽场景带重试）。安装位置随
# 基础镜像变化（runner 的 node 工具缓存 /opt/acttoolcache/...、npm 全局前缀或
# /root/.local/bin）；统一软链到 /usr/local/bin，保证非 root 运行 daemon 时也能
# 在受限 PATH 下解析到 claude
RUN set -eu; \
    npm install -g @anthropic-ai/claude-code --registry=https://registry.npmjs.org/ \
      || npm install -g @anthropic-ai/claude-code --registry=https://registry.npmjs.org/; \
    if ! command -v claude &> /dev/null; then \
      curl -fsSL https://claude.ai/cli/install.sh | sh || true; \
    fi; \
    claude_bin="$(command -v claude || true)"; \
    if [ -n "$claude_bin" ] && [ "$claude_bin" != "/usr/local/bin/claude" ]; then \
      ln -sfn "$claude_bin" /usr/local/bin/claude; \
    fi; \
    command -v claude &> /dev/null && claude --version || echo "Claude Code installation pending"

# Go 工具链（评审 Go 仓库时可直接 go build / go test）
COPY --from=golang:1.27 /usr/local/go /usr/local/go

# 缓存目录统一到 ~，便于整体挂载/缓存
ENV PATH=/usr/local/go/bin:/root/go/bin:/root/.bun/bin:/root/.local/bin:${PATH} \
    GOPATH=/root/go \
    GOROOT=/usr/local/go \
    GOCACHE=/root/.cache/go-build \
    GOMODCACHE=/root/go/pkg/mod \
    CARGO_HOME=/root/.cargo \
    RUSTUP_HOME=/root/.rustup \
    BUN_INSTALL_CACHE_DIR=/root/.cache/bun \
    npm_config_cache=/root/.cache/npm \
    PIP_CACHE_DIR=/root/.cache/pip \
    UV_CACHE_DIR=/root/.cache/uv \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TMPDIR=/tmp

RUN mkdir -p /root/go/bin /root/go/pkg/mod /root/.cache/go-build /root/.cache/bun \
    /root/.cache/npm /root/.cache/pip /root/.cache/uv

CMD ["bash"]

# 评审会话镜像（会话容器形态）
FROM review-env AS review

# daemon 镜像：先构建 assistant 二进制，再叠加到评审环境
FROM golang:1.27 AS assistant-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /assistant ./cmd/assistant

FROM review-env AS daemon
COPY --from=assistant-build /assistant /usr/local/bin/assistant
# 配置/克隆/日志都在挂载的用户目录下；worktree 在 /tmp（compose 用 tmpfs 提供）
ENTRYPOINT ["/usr/local/bin/assistant"]
CMD ["run"]
