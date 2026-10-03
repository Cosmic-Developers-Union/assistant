#!/usr/bin/env bash
# 起测试环境（docker-compose.test.yaml）并把 e2e 需要的端点/凭据写进 test/e2e/.env。
#
#   ./test/seed.sh          # 全量：gitea + minio + runner，并建管理员与访问令牌
#   ./test/seed.sh minio    # 只起 minio（S3 归档用例）
#
# 清理（连 act_runner 的 job 容器与任务卷一起清）：
#   ./test/cleanup.sh     或   make test-env-down
#
# 服务与镜像都定义在仓库根部的 docker-compose.test.yaml，本脚本只做三件事：清掉
# 上一次的现场、把服务拉起来、再把凭据写进 .env。每次调用都从零开始（测试环境是
# 可丢弃的，数据在 tmpfs），所以起环境是幂等的。runner 的注册令牌由 runner 容器
# 自己用管理员账号换（test/runner-register.sh），不必先起 gitea 再起 runner。
#
# .env 按「本次起了哪些服务」整份重写（不做键合并）：只起 minio 时，Gitea 用例会
# 如实 SKIP，而不是拿着上一次的过期地址去连。
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f docker-compose.test.yaml)
MODE="${1:-full}"

HTTP_PORT="${ASSISTANT_E2E_GITEA_HTTP_PORT:-3300}"
HOST="${ASSISTANT_E2E_HOST:-http://127.0.0.1:$HTTP_PORT}"
ADMIN_USER="${ASSISTANT_E2E_ADMIN_USER:-e2eadmin}"
ADMIN_PASSWORD="${ASSISTANT_E2E_ADMIN_PASSWORD:-admin-e2e-password}"
ADMIN_EMAIL="${ASSISTANT_E2E_ADMIN_EMAIL:-admin@assistant.local}"
IMAGE="${ASSISTANT_E2E_IMAGE:-assistant:e2e}"
MINIO_ENDPOINT="${ASSISTANT_E2E_MINIO_ENDPOINT:-127.0.0.1:${MINIO_HOST_PORT:-9000}}"
MINIO_BUCKET="${ASSISTANT_E2E_MINIO_BUCKET:-assistant-sessions}"
MINIO_ACCESS_KEY="${MINIO_ROOT_USER:-minioadmin}"
MINIO_SECRET_KEY="${MINIO_ROOT_PASSWORD:-minioadmin}"

command -v docker >/dev/null 2>&1 || {
  echo "需要 docker（含 compose 插件）" >&2
  exit 1
}
"${COMPOSE[@]}" version >/dev/null 2>&1 || {
  echo "需要 docker compose 插件（docker compose version 不可用）" >&2
  exit 1
}

case "$MODE" in
full | minio) ;;
*)
  echo "用法：$0 [minio]（缺省全量 gitea + minio + runner）" >&2
  exit 2
  ;;
esac

# minio_env_key_values 输出 MinIO 那几行（两种模式共用）
minio_env_key_values() {
  printf 'ASSISTANT_E2E_MINIO_ENDPOINT=%s\n' "$MINIO_ENDPOINT"
  printf 'ASSISTANT_E2E_MINIO_BUCKET=%s\n' "$MINIO_BUCKET"
  printf 'ASSISTANT_E2E_MINIO_ACCESS_KEY=%s\n' "$MINIO_ACCESS_KEY"
  printf 'ASSISTANT_E2E_MINIO_SECRET_KEY=%s\n' "$MINIO_SECRET_KEY"
}

# 每次从零开始：测试环境可丢弃，先按 test/cleanup.sh 清掉上一次的现场（含 act_runner
# 的 job 容器与任务卷；数据在 tmpfs，随容器消失）。否则上次留下的仓库/账号/令牌会串
# 进本次运行，出现「令牌名已存在」这类与代码无关的失败。
./test/cleanup.sh >/dev/null 2>&1 || true

case "$MODE" in
minio)
  echo "==> 启动临时 MinIO"
  if ! "${COMPOSE[@]}" up -d --wait --wait-timeout 180 minio; then
    echo "MinIO 未在超时内就绪，容器状态与日志尾部：" >&2
    "${COMPOSE[@]}" ps >&2 || true
    "${COMPOSE[@]}" logs --tail 50 minio >&2 || true
    exit 1
  fi
  mkdir -p test/e2e
  minio_env_key_values >test/e2e/.env
  # 桶由 e2e 用例创建并删除，运行时不改变远端桶配置
  echo "==> 就绪：http://$MINIO_ENDPOINT（桶 $MINIO_BUCKET 由测试自建）"
  ;;

full)
  echo "==> 构建 job 镜像 $IMAGE（仓库 workflow 的容器 job 用）"
  docker build -q -t "$IMAGE" . >/dev/null

  echo "==> 启动 gitea + minio + runner"
  if ! "${COMPOSE[@]}" up -d --wait --wait-timeout 180; then
    echo "测试环境未在超时内就绪，容器状态与日志尾部：" >&2
    "${COMPOSE[@]}" ps >&2 || true
    "${COMPOSE[@]}" logs --tail 50 >&2 || true
    exit 1
  fi

  echo "==> 创建管理员 @$ADMIN_USER（已存在则忽略）"
  "${COMPOSE[@]}" exec -T --user git gitea gitea admin user create \
    --username "$ADMIN_USER" \
    --password "$ADMIN_PASSWORD" \
    --email "$ADMIN_EMAIL" \
    --admin \
    --must-change-password=false >/dev/null 2>&1 || true

  echo "==> 生成管理员访问令牌"
  TOKEN="$("${COMPOSE[@]}" exec -T --user git gitea gitea admin user generate-access-token \
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
    TOKEN="$("${COMPOSE[@]}" exec -T gitea curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" \
      -X POST -H 'Content-Type: application/json' \
      -d '{"name":"e2e-basic","scopes":["all"]}' \
      "http://127.0.0.1:3000/api/v1/users/$ADMIN_USER/tokens" \
      | sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p')"
  fi
  if [ -z "$TOKEN" ] || [ "${#TOKEN}" -lt 20 ]; then
    echo "生成管理员令牌失败" >&2
    exit 1
  fi

  mkdir -p test/e2e
  {
    printf 'ASSISTANT_E2E_HOST=%s\n' "$HOST"
    printf 'ASSISTANT_E2E_ADMIN_USER=%s\n' "$ADMIN_USER"
    printf 'ASSISTANT_E2E_ADMIN_PASSWORD=%s\n' "$ADMIN_PASSWORD"
    printf 'ASSISTANT_E2E_ADMIN_TOKEN=%s\n' "$TOKEN"
    printf 'ASSISTANT_E2E_IMAGE=%s\n' "$IMAGE"
    minio_env_key_values
  } >test/e2e/.env

  # 管理员刚建好，runner 容器那边会自己换令牌注册（见 test/runner-register.sh）
  echo "==> 就绪：$HOST（runner 自注册，job 镜像 $IMAGE）"
  ;;
esac
