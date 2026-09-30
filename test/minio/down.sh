#!/usr/bin/env bash
# 停止并清除临时 MinIO：docker 模式的容器与数据卷，或 source 模式的本地进程。
set -euo pipefail
cd "$(dirname "$0")"

if [[ -f .cache/minio.pid ]]; then
  pid="$(cat .cache/minio.pid)"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 20); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
    echo "==> 已停止源码模式 MinIO（pid $pid）"
  fi
  rm -f .cache/minio.pid
fi

mode=""
if [[ -f .cache/mode ]]; then
  mode="$(cat .cache/mode)"
fi
rm -f .cache/mode

# source 模式没起过容器，不必再对 compose 项目下手；docker 模式与旧现场照常清理。
if [[ "$mode" != "source" ]]; then
  docker compose down -v --remove-orphans
fi
