#!/bin/sh
# act_runner 容器的入口包装：镜像本身不带注册令牌，这里先用管理员账号从 Gitea 换一个
# 注册令牌，再交给镜像自带的 run.sh 去注册并启动守护进程。
#
# 为什么要在容器里等：Gitea 的 runner 注册令牌只能现生成（没有可写死的静态配置），
# 而 `docker compose up` 会把 gitea 与 runner 一起拉起来——顺序只能由容器自己等：
# 管理员账号由 test/seed.sh 在 Gitea 就绪后创建，这里会一直重试到它出现为止。
# 这样起环境就只需要一条 `docker compose up -d --wait`。
#
# 环境变量（见 docker-compose.test.yaml）：
#   GITEA_INSTANCE_URL      Gitea 地址（compose 网络内）
#   SEED_ADMIN_USER/PASSWORD  test/seed.sh 建的管理员账号
set -eu

until wget -q -O /dev/null "$GITEA_INSTANCE_URL/api/v1/version" 2>/dev/null; do
  sleep 2
done

# 镜像里是 busybox wget，没有 --user/--password，用 Authorization 头带 Basic 凭据
auth="$(printf '%s:%s' "$SEED_ADMIN_USER" "$SEED_ADMIN_PASSWORD" | base64 | tr -d '\n')"
while :; do
  body="$(wget -q -O - \
    --header="Content-Type: application/json" \
    --header="Authorization: Basic $auth" \
    --post-data="{}" \
    "$GITEA_INSTANCE_URL/api/v1/admin/actions/runners/registration-token" 2>/dev/null)" || true
  token="$(printf '%s' "$body" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  [ -n "$token" ] && break
  sleep 2
done

GITEA_RUNNER_REGISTRATION_TOKEN="$token"
export GITEA_RUNNER_REGISTRATION_TOKEN
exec /usr/local/bin/run.sh
