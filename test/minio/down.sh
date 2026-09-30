#!/usr/bin/env bash
# 停止并清除临时 MinIO（容器与数据卷）。
set -euo pipefail
cd "$(dirname "$0")"
docker compose down -v --remove-orphans
