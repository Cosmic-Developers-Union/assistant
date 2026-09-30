#!/usr/bin/env bash
# 启动临时 MinIO，并把端点与密钥写入 test/e2e/.env。
#
# 两种后端，用 ASSISTANT_MINIO_MODE 选择（默认 auto）：
#   docker —— docker compose 起 ${MINIO_IMAGE}（CI 与有外网时的默认做法）
#   source —— go install 从源码编译真 MinIO，以本地进程跑（拉不到镜像时用）
#   auto   —— 镜像本地已有或能拉下来就走 docker，否则回退 source
#
# source 模式拉不到预编译镜像也能跑：模块走 GOPROXY（缺省给一个国内可用的只读代理，
# 已有设置不覆盖）。二进制、数据、日志都落在本目录的 .cache/（已被 .gitignore 忽略）。
set -euo pipefail
cd "$(dirname "$0")"
source ../envfile.sh

ENDPOINT="${ASSISTANT_E2E_MINIO_ENDPOINT:-127.0.0.1:9000}"
BUCKET="${ASSISTANT_E2E_MINIO_BUCKET:-assistant-sessions}"
ACCESS_KEY="${MINIO_ROOT_USER:-minioadmin}"
SECRET_KEY="${MINIO_ROOT_PASSWORD:-minioadmin}"
IMAGE="${MINIO_IMAGE:-pgsty/minio:latest}"
CONSOLE="${ASSISTANT_E2E_MINIO_CONSOLE:-127.0.0.1:9001}"
MODE="${ASSISTANT_MINIO_MODE:-auto}"

CACHE=".cache"
BIN="$CACHE/bin/minio"
DATA="$CACHE/data"
LOG="$CACHE/minio.log"
PIDFILE="$CACHE/minio.pid"
MODE_FILE="$CACHE/mode"
# source 模式的 MinIO 版本（RELEASE 标签，goproxy 上可解析）；要换版本覆盖它。
MINIO_SOURCE_VERSION="${MINIO_SOURCE_VERSION:-RELEASE.2025-04-22T22-12-26Z}"

# 拉镜像时最多等多久再判定「拉不到」而回退源码模式（秒）。
MINIO_PULL_TIMEOUT="${MINIO_PULL_TIMEOUT:-30}"

wait_ready() {
  echo "==> 等待 MinIO 就绪：$ENDPOINT"
  for _ in $(seq 1 90); do
    if curl -fsS "http://$ENDPOINT/minio/health/live" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

up_docker() {
  echo "==> 启动临时 MinIO（docker，镜像 $IMAGE）"
  # 不用 --wait：就绪由上面的宿主探测决定，不依赖镜像里的 healthcheck 工具
  docker compose up -d minio
}

up_source() {
  if [[ ! -x "$BIN" ]]; then
    command -v go >/dev/null 2>&1 || {
      echo "source 模式需要 go（且首次编译要能访问模块代理）" >&2
      exit 1
    }
    echo "==> 从源码编译 MinIO $MINIO_SOURCE_VERSION（首次约几分钟）"
    mkdir -p "$CACHE/bin"
    GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" GOBIN="$PWD/$CACHE/bin" \
      go install "github.com/minio/minio@$MINIO_SOURCE_VERSION"
  fi
  echo "==> 启动临时 MinIO（source，$BIN）"
  mkdir -p "$DATA"
  MINIO_ROOT_USER="$ACCESS_KEY" MINIO_ROOT_PASSWORD="$SECRET_KEY" \
    "$BIN" server "$DATA" --address "$ENDPOINT" --console-address "$CONSOLE" \
    >>"$LOG" 2>&1 &
  echo $! >"$PIDFILE"
}

# 镜像本地已有，或能在超时内拉下来，就用 docker；否则回退源码模式。
resolve_auto_mode() {
  if docker image inspect "$IMAGE" >/dev/null 2>&1; then
    MODE=docker
    return
  fi
  local pull=(docker pull "$IMAGE")
  if command -v timeout >/dev/null 2>&1; then
    pull=(timeout "$MINIO_PULL_TIMEOUT" docker pull "$IMAGE")
  fi
  if "${pull[@]}" >/dev/null 2>&1; then
    MODE=docker
    return
  fi
  echo "==> 拉不到镜像 $IMAGE，回退源码模式" >&2
  MODE=source
}

case "$MODE" in
docker) ;;
source) ;;
auto) resolve_auto_mode ;;
*)
  echo "ASSISTANT_MINIO_MODE 只能是 auto/docker/source（当前：$MODE）" >&2
  exit 2
  ;;
esac

case "$MODE" in
docker) up_docker ;;
source) up_source ;;
esac

if ! wait_ready; then
  case "$MODE" in
  docker) echo "MinIO 未在超时内就绪：$(docker compose ps)" >&2 ;;
  source)
    echo "MinIO 未在超时内就绪，日志尾部（$LOG）：" >&2
    tail -20 "$LOG" >&2 || true
    ;;
  esac
  exit 1
fi

mkdir -p ../e2e
envfile_set ../e2e/.env \
  "ASSISTANT_E2E_MINIO_ENDPOINT=$ENDPOINT" \
  "ASSISTANT_E2E_MINIO_BUCKET=$BUCKET" \
  "ASSISTANT_E2E_MINIO_ACCESS_KEY=$ACCESS_KEY" \
  "ASSISTANT_E2E_MINIO_SECRET_KEY=$SECRET_KEY"
mkdir -p "$CACHE"
echo "$MODE" >"$MODE_FILE"

# 桶由 assistant 归档启动时自建（EnsureBucket），这里不预建，顺带验证该路径
echo "==> 就绪：http://$ENDPOINT（桶 $BUCKET 由测试自建，模式 $MODE）"
