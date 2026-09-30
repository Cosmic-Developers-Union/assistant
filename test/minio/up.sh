#!/usr/bin/env bash
# 启动临时 MinIO（docker compose，社区镜像）并把端点与密钥写入 test/e2e/.env。
#
# 依赖面只有 docker + compose 插件：镜像在 docker-compose.yaml 里按「版本标签 +
# 摘要」固定，就绪判定用镜像自带的 healthcheck（curl /minio/health/live），现场不
# 编译 MinIO，也不需要宿主机装 go / curl。
#
# 可覆盖的环境变量：
#   MINIO_IMAGE          换镜像（须自带上面的 healthcheck 探针）
#   MINIO_HOST_PORT      宿主端口，缺省 9000（与写进 .env 的端点保持一致）
#   MINIO_CONSOLE_PORT   控制台宿主端口，缺省 9001
#   MINIO_ROOT_USER / MINIO_ROOT_PASSWORD   根凭据，缺省 minioadmin/minioadmin
#   ASSISTANT_E2E_MINIO_ENDPOINT / _BUCKET  写进 .env 的连接目标与桶名（缺省本机端口 + assistant-sessions）
set -euo pipefail
cd "$(dirname "$0")"
source ../envfile.sh

PORT="${MINIO_HOST_PORT:-9000}"
CONSOLE_PORT="${MINIO_CONSOLE_PORT:-9001}"
ENDPOINT="${ASSISTANT_E2E_MINIO_ENDPOINT:-127.0.0.1:$PORT}"
BUCKET="${ASSISTANT_E2E_MINIO_BUCKET:-assistant-sessions}"
ACCESS_KEY="${MINIO_ROOT_USER:-minioadmin}"
SECRET_KEY="${MINIO_ROOT_PASSWORD:-minioadmin}"

command -v docker >/dev/null 2>&1 || {
  echo "需要 docker 才能起临时 MinIO（本测试不再从源码编译 MinIO）" >&2
  exit 1
}
docker compose version >/dev/null 2>&1 || {
  echo "需要 docker compose 插件（docker compose version 不可用）" >&2
  exit 1
}

echo "==> 启动临时 MinIO（docker compose，--wait 等镜像自带 healthcheck）"
if ! docker compose up -d --wait --wait-timeout 120 minio; then
  echo "MinIO 未在超时内就绪，容器状态与日志尾部：" >&2
  docker compose ps >&2 || true
  docker compose logs --tail 50 minio >&2 || true
  exit 1
fi

mkdir -p ../e2e
envfile_set ../e2e/.env \
  "ASSISTANT_E2E_MINIO_ENDPOINT=$ENDPOINT" \
  "ASSISTANT_E2E_MINIO_BUCKET=$BUCKET" \
  "ASSISTANT_E2E_MINIO_ACCESS_KEY=$ACCESS_KEY" \
  "ASSISTANT_E2E_MINIO_SECRET_KEY=$SECRET_KEY"

# 桶由 assistant 归档启动时自建（EnsureBucket），这里不预建，顺带验证该路径
echo "==> 就绪：http://$ENDPOINT（控制台 http://127.0.0.1:$CONSOLE_PORT，桶 $BUCKET 由测试自建）"
echo "==> 镜像：$(docker compose ps --format '{{.Image}}' minio)"
