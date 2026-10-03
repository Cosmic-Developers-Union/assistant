# syntax=docker/dockerfile:1

# 静态单二进制镜像：既供 action.yml 从固定源码构建，也可独立发布供 CLI 使用。
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/Cosmic-Developers-Union/assistant/internal/cli.version=${VERSION}" \
    -o /assistant ./cmd/assistant

# 保留 shell 以兼容独立镜像的 run 步骤；源码 Action 直接执行 ENTRYPOINT。
# Action 只通过环境访问 Gitea API，不读取或写入 runner 挂载的项目文件。
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /assistant /usr/local/bin/assistant
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/assistant"]
