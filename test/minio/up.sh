#!/usr/bin/env bash
# 启动临时 MinIO（docker compose），并把端点与密钥写入 test/e2e/.env。
set -euo pipefail
cd "$(dirname "$0")"

ENDPOINT="${ASSISTANT_E2E_MINIO_ENDPOINT:-127.0.0.1:9000}"
BUCKET="${ASSISTANT_E2E_MINIO_BUCKET:-assistant-sessions}"
ACCESS_KEY="${MINIO_ROOT_USER:-minioadmin}"
SECRET_KEY="${MINIO_ROOT_PASSWORD:-minioadmin}"

echo "==> 启动临时 MinIO"
# 镜像可覆盖：MINIO_IMAGE=adobe/s3mock:latest ./test/minio/up.sh
# 不用 --wait：就绪由下面的宿主探测决定，不依赖镜像里的 healthcheck 工具
docker compose up -d minio

echo "==> 等待 MinIO 就绪：$ENDPOINT"
for _ in $(seq 1 90); do
  if curl -fsS "http://$ENDPOINT/minio/health/live" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -fsS "http://$ENDPOINT/minio/health/live" >/dev/null 2>&1; then
  echo "MinIO 未在超时内就绪：$(docker compose ps)" >&2
  exit 1
fi

# 桶由 assistant 归档启动时自建（EnsureBucket），这里不预建，顺带验证该路径

mkdir -p ../e2e
{
  echo "ASSISTANT_E2E_MINIO_ENDPOINT=$ENDPOINT"
  echo "ASSISTANT_E2E_MINIO_BUCKET=$BUCKET"
  echo "ASSISTANT_E2E_MINIO_ACCESS_KEY=$ACCESS_KEY"
  echo "ASSISTANT_E2E_MINIO_SECRET_KEY=$SECRET_KEY"
} > ../e2e/.env

echo "==> 就绪：http://$ENDPOINT（桶 $BUCKET 由测试自建）"
