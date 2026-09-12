#!/usr/bin/env bash
# 启动临时 Gitea（docker compose），创建管理员并生成访问令牌，
# 把 ASSISTANT_E2E_HOST / ASSISTANT_E2E_ADMIN_TOKEN 写入 test/e2e/.env。
set -euo pipefail
cd "$(dirname "$0")"

HOST="${ASSISTANT_E2E_HOST:-http://127.0.0.1:3300}"
ADMIN_USER="${ASSISTANT_E2E_ADMIN_USER:-e2eadmin}"
ADMIN_PASSWORD="${ASSISTANT_E2E_ADMIN_PASSWORD:-admin-e2e-password}"
ADMIN_EMAIL="${ASSISTANT_E2E_ADMIN_EMAIL:-admin@assistant.local}"

docker compose up -d --wait

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

mkdir -p ../e2e
cat > ../e2e/.env <<EOF
ASSISTANT_E2E_HOST=$HOST
ASSISTANT_E2E_ADMIN_TOKEN=$TOKEN
EOF

echo "==> 就绪：$HOST（令牌已写入 test/e2e/.env）"
