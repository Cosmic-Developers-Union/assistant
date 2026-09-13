#!/usr/bin/env bash
# 启动临时 Gitea + act_runner（docker compose），创建管理员并生成访问令牌，
# 并构建本仓库 assistant 镜像供容器 job 使用（runner 标签与 workflow 的
# container 都指向该镜像）。凭据写入 test/e2e/.env，compose 变量写入本目录 .env。
set -euo pipefail
cd "$(dirname "$0")"

HOST="${ASSISTANT_E2E_HOST:-http://127.0.0.1:3300}"
ADMIN_USER="${ASSISTANT_E2E_ADMIN_USER:-e2eadmin}"
ADMIN_PASSWORD="${ASSISTANT_E2E_ADMIN_PASSWORD:-admin-e2e-password}"
ADMIN_EMAIL="${ASSISTANT_E2E_ADMIN_EMAIL:-admin@assistant.local}"
IMAGE="${ASSISTANT_E2E_IMAGE:-assistant:e2e}"

echo "==> 构建 assistant 镜像 $IMAGE（runner 容器 job 用）"
docker build -q -t "$IMAGE" ../.. >/dev/null

echo "==> 启动临时 Gitea"
docker compose up -d --wait gitea

echo "==> 等待 Gitea API 就绪：$HOST"
for _ in $(seq 1 60); do
  if curl -fsS "$HOST/api/v1/version" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "==> 创建管理员 @$ADMIN_USER（已存在则忽略）"
docker compose exec -T --user git gitea gitea admin user create \
  --username "$ADMIN_USER" \
  --password "$ADMIN_PASSWORD" \
  --email "$ADMIN_EMAIL" \
  --admin \
  --must-change-password=false >/dev/null 2>&1 || true

echo "==> 生成管理员访问令牌"
TOKEN="$(docker compose exec -T --user git gitea gitea admin user generate-access-token \
  --username "$ADMIN_USER" \
  --token-name "e2e-$(date +%s)" \
  --scopes all \
  --raw 2>/dev/null | tr -d '\r\n' || true)"
# --raw 输出应仅为令牌本身；含空白则视为失败
case "$TOKEN" in
  *[[:space:]]*) TOKEN="" ;;
esac

if [ -z "$TOKEN" ]; then
  echo "==> CLI 生成失败，回退 Basic Auth API"
  TOKEN="$(curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" \
    -X POST -H 'Content-Type: application/json' \
    -d '{"name":"e2e-basic","scopes":["all"]}' \
    "$HOST/api/v1/users/$ADMIN_USER/tokens" \
    | sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p')"
fi
if [ -z "$TOKEN" ] || [ "${#TOKEN}" -lt 20 ]; then
  echo "生成管理员令牌失败" >&2
  exit 1
fi

echo "==> 生成 act_runner 注册令牌"
RUNNER_TOKEN="$(docker compose exec -T --user git gitea gitea actions generate-runner-token | tr -d '\r\n')"
if [ -z "$RUNNER_TOKEN" ]; then
  echo "生成 runner 注册令牌失败" >&2
  exit 1
fi
cat > .env <<EOF
RUNNER_TOKEN=$RUNNER_TOKEN
ASSISTANT_E2E_IMAGE=$IMAGE
EOF

echo "==> 启动 act_runner"
docker compose up -d --wait runner

mkdir -p ../e2e
cat > ../e2e/.env <<EOF
ASSISTANT_E2E_HOST=$HOST
ASSISTANT_E2E_ADMIN_USER=$ADMIN_USER
ASSISTANT_E2E_ADMIN_PASSWORD=$ADMIN_PASSWORD
ASSISTANT_E2E_ADMIN_TOKEN=$TOKEN
ASSISTANT_E2E_IMAGE=$IMAGE
EOF

echo "==> 就绪：$HOST（act_runner + 镜像 $IMAGE）"
