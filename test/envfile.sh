#!/usr/bin/env bash
# 把 KEY=VALUE 写进 .env 风格的键值文件：覆盖同名键、保留其它键。
#
# 为什么需要它：test/gitea 与 test/minio 的 up.sh 都往 test/e2e/.env 写自己那部分
# 配置（Gitea 的 HOST/令牌、MinIO 的端点/密钥），而 e2e 测试只读这一个文件。用 `>`
# 直接截断会让后写的那个把先写的整段抹掉——make test-e2e 先起 Gitea 后起 MinIO，
# 结果 Gitea 测试拿不到 HOST/TOKEN 全部 SKIP，「绿」得名不副实。

# envfile_set FILE KEY=VALUE...
envfile_set() {
  local file="$1"
  shift
  local keys=""
  local kv
  for kv in "$@"; do
    keys+="${kv%%=*}"$'\n'
  done

  local content=""
  if [[ -f "$file" ]]; then
    content="$(cat "$file")"
  fi

  local out=""
  local line key
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    key="${line%%=*}"
    if ! grep -qxF "$key" <<<"$keys"; then
      out+="$line"$'\n'
    fi
  done <<<"$content"

  printf '%s' "$out" >"$file"
  printf '%s\n' "$@" >>"$file"
}
