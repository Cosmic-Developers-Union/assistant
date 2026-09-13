# syntax=docker/dockerfile:1

# 静态单二进制镜像：供 Gitea/GitHub Actions 的仓库级 workflow 直接以
# `container: ghcr.io/<owner>/assistant:<tag>` 运行 sync / automerge 等命令。
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /assistant ./cmd/assistant

# 带 shell 的最小运行镜像：仓库 workflow 的 run 步骤由 act_runner 以
# `sh -c` 执行（distroless 无 /bin/sh，run 步骤无法运行），因此使用 alpine。
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /assistant /usr/local/bin/assistant
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/assistant"]
