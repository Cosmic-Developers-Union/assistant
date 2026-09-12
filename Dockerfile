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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /assistant /usr/local/bin/assistant
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/assistant"]
