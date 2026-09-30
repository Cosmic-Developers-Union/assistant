#!/usr/bin/env bash
# 清理测试环境：先删 act_runner 的 job 容器与任务卷，再 down 掉 compose 栈。
#
# 为什么不能只 `docker compose down`：workflow 的 job 容器是 act_runner 用 docker API
# 直接建的，不属于 compose 项目（容器上没有任何 compose 标签），compose 既不认它们，
# 也删不掉它们占着的默认网络；任务卷（GITEA-ACTIONS-TASK-*）同理。只 down 的话，
# 现场会留下还在跑的 job 容器、连带着网络和任务卷一起残留。
#
# act_runner 的 act-toolcache 卷是跨运行复用的工具缓存（不属于单次现场），这里不动。
set -euo pipefail
cd "$(dirname "$0")/.."

jobs="$(docker ps -aq --filter name=GITEA-ACTIONS-TASK- || true)"
[ -z "$jobs" ] || docker rm -f $jobs >/dev/null 2>&1 || true

docker compose -f docker-compose.test.yaml down --remove-orphans

vols="$(docker volume ls -qf name=GITEA-ACTIONS-TASK- || true)"
[ -z "$vols" ] || docker volume rm -f $vols >/dev/null 2>&1 || true

rm -f test/e2e/.env
