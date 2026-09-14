# syntax=docker/dockerfile:1
# daemon 镜像：评审镜像（claude CLI + git/Go 等工具链）叠加 assistant 自身二进制，
# 供 docker-compose.yaml 以容器形态常驻 `assistant run`（调度 + 状态 API + 微信桥）。
ARG REVIEW_IMAGE=ghcr.io/cosmic-developers-union/assistant-review:latest

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /assistant ./cmd/assistant

FROM ${REVIEW_IMAGE}
COPY --from=build /assistant /usr/local/bin/assistant
# 配置/克隆/日志都在挂载的 HOME 下；worktree 在 /tmp（compose 用 tmpfs 提供）
ENTRYPOINT ["/usr/local/bin/assistant"]
CMD ["run"]
