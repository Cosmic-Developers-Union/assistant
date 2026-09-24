#!/usr/bin/env bash
# install.sh —— assistant 主机安装，按平台两种形态：
#
#   macOS（测试形态）：二进制 + 空配置骨架。以当前用户安装、直接前台跑
#     `assistant run`——不建服务用户/systemd（个人测试机无需隔离）；配置落
#     ~/Library/Application Support/Cosmic-Developers-Union/assistant（Go 的
#     显式模式：配置写在运行命令时所在的目录（./config.json）。
#
#   Linux（服务形态）：独立系统服务用户（nologin、无密码，无需登陆）+
#     XDG 标准目录 + systemd 单元 + 空配置骨架，`assistant run` 由 systemd
#     常驻运行。
#
# 二进制来源优先级：--bin > 检出内现成的 ./assistant > go 构建 >
# GitHub Release 下载（按 OS/ARCH 选 linux/darwin × amd64/arm64）。
# 支持 curl -fsSL <raw-url> | bash 一键执行；幂等。
set -euo pipefail

GITHUB_REPO=Cosmic-Developers-Union/assistant
RAW_SCRIPT_URL=https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh
RELEASE_BASE=https://github.com/$GITHUB_REPO/releases/latest/download

usage() {
  cat <<'EOF'
用法: install.sh [选项]
      curl -fsSL https://raw.githubusercontent.com/Cosmic-Developers-Union/assistant/main/install.sh | bash

主机安装 assistant：
  macOS  测试形态——二进制 + 空配置（当前用户，前台跑 assistant run）
  Linux  服务形态——独立系统服务用户 + 工作目录 + systemd 单元 + 空配置

二进制来源优先级：--bin 指定 > 检出内现成的 ./assistant > go 构建 > GitHub Release 下载。

选项:
  --user NAME      服务用户名（仅 Linux；缺省 assistant）
  --home PATH      服务用户家目录（仅 Linux；缺省 /var/lib/<user>）
  --bin PATH       使用已构建的二进制（跳过构建/下载）
  --prefix PATH    二进制安装前缀（缺省 /usr/local，装到 <prefix>/bin/assistant）
  --systemd        强制安装 systemd 单元（仅 Linux）
  --no-systemd     不安装 systemd 单元（缺省 auto：检测到运行中的 systemd 才装）
  --enable         装完后 systemctl enable（仅 Linux）
  -h, --help       显示本帮助

示例:
  curl -fsSL https://raw.githubusercontent.com/Cosmic-Developers-Union/assistant/main/install.sh | bash
  sudo ./install.sh --enable                 # Linux 检出内安装 + 设开机自启
  ./install.sh --bin ./assistant             # macOS 检出内安装（无需 sudo）
EOF
}

SERVICE_USER=assistant
SERVICE_HOME=
BIN_SOURCE=
PREFIX=/usr/local
INSTALL_SYSTEMD=auto   # auto|yes|no
ENABLE=false

# 脚本自身路径：curl | bash 时 $0 指向的是解释器/宿主而非本脚本（stdin 执行），
# 用内容标记区分——$0 的文件开头含本脚本标记才算文件模式，否则按 stdin 模式
# （SCRIPT_DIR 为空，二进制走 Release 下载）。
SELF=$(readlink -f "$0" 2>/dev/null || true)
if [ -n "$SELF" ] && { [ ! -f "$SELF" ] ||
    ! head -c 4096 "$SELF" 2>/dev/null | grep -q '^GITHUB_REPO=Cosmic-Developers-Union/assistant$'; }; then
  SELF=
fi
SCRIPT_DIR=
[ -n "$SELF" ] && SCRIPT_DIR=$(dirname "$SELF")
REPO_MODE=false
[ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/cmd/assistant/main.go" ] && REPO_MODE=true

while [ $# -gt 0 ]; do
  case "$1" in
    --user) SERVICE_USER=$2; shift 2 ;;
    --home) SERVICE_HOME=$2; shift 2 ;;
    --bin) BIN_SOURCE=$2; shift 2 ;;
    --prefix) PREFIX=$2; shift 2 ;;
    --systemd) INSTALL_SYSTEMD=yes; shift ;;
    --no-systemd) INSTALL_SYSTEMD=no; shift ;;
    --enable) ENABLE=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1（--help 查看用法）" >&2; exit 2 ;;
  esac
done

OS=$(uname -s)
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "错误: 不支持的架构 $(uname -m)（用 --bin 指定二进制）" >&2; exit 1 ;;
esac

die() { echo "错误: $*" >&2; exit 1; }
# 进度日志走 stderr：resolve_binary 的 stdout 被命令替换捕获为二进制路径，
# 日志混进去会把多行文本当路径传给 install
step() { echo "==> $*" >&2; }

# fetch 下载到本地文件（curl 优先，wget 兜底）
fetch() { # fetch <url> -o <path>
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$3"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$3" "$1"
  else
    die "需要 curl 或 wget"
  fi
}

# ---------- 二进制来源 ----------
download_release() {
  url="$RELEASE_BASE/assistant-$(echo "$OS" | tr '[:upper:]' '[:lower:]')-$ARCH"
  step "下载 Release 二进制: $url"
  downloaded=$(mktemp /tmp/assistant-XXXXXX)
  fetch "$url" -o "$downloaded"
  chmod +x "$downloaded"
  echo "$downloaded"
}

resolve_binary() {
  if [ -n "$BIN_SOURCE" ]; then
    [ -x "$BIN_SOURCE" ] || die "--bin $BIN_SOURCE 不存在或不可执行"
    echo "$BIN_SOURCE"
    return
  fi
  if [ "$REPO_MODE" = true ]; then
    if [ -x "$SCRIPT_DIR/assistant" ]; then
      echo "$SCRIPT_DIR/assistant"
      return
    fi
    if command -v go >/dev/null 2>&1; then
      step "未找到预构建二进制，用 go 构建（$OS/$ARCH）"
      version=$(git -C "$SCRIPT_DIR" describe --tags --always 2>/dev/null || echo dev)
      (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build \
        -ldflags "-w -s -X main.version=$version" \
        -o "$SCRIPT_DIR/assistant" ./cmd/assistant)
      echo "$SCRIPT_DIR/assistant"
      return
    fi
  fi
  download_release
}

BINDIR=$PREFIX/bin

print_next_steps() { # 公共尾部：补全配置并跑 run 的说明 <bin> <work_dir>
  echo "下一步（token 已备好时最快路径——环境变量，不用写文件）："
  echo "  export GITEA_HOST=https://<gitea地址> GITEA_ACCESS_TOKEN=<token>"
  echo "  $1 run                          # 前台跑，Ctrl+C 停止；--debug 看决策细节"
  echo "  # AI 供应商 key：编辑 $2/config.json 的 providers；通道与运行时看 channels/runtimes"
  echo "  # 没有 config.json 时自动按环境变量单实例运行"
  echo "  # run 需要 claude CLI 在 PATH（macOS: npm i -g @anthropic-ai/claude-code 等）"
}

# ============================================================
# macOS —— 测试形态：当前用户、前台跑 run，不建服务用户/systemd
# ============================================================
install_darwin() {
  [ -z "$SERVICE_HOME" ] || step "提示: macOS 测试形态不使用 --home（--user/--systemd/--enable 同样忽略）"

  local binary
  binary=$(resolve_binary)
  step "二进制: $binary"

  # 显式模式：配置写在你运行安装命令时所在的目录（./config.json）
  local work_dir=$PWD

  # 安装二进制：目标目录可写直接装，否则单点 sudo（不整脚本提权，配置仍归当前用户）
  local sudo=
  if [ -e "$BINDIR" ]; then
    [ -w "$BINDIR" ] || sudo=sudo
  else
    [ -w "$(dirname "$BINDIR")" ] || sudo=sudo
  fi
  step "安装二进制到 $BINDIR/assistant"
  $sudo install -d "$BINDIR"
  $sudo install -m 0755 "$binary" "$BINDIR/assistant"

  # 空配置骨架（当前用户，落在当前目录）
  if [ -f "$work_dir/config.json" ]; then
    step "配置已存在，跳过生成: $work_dir/config.json"
  else
    step "生成空配置骨架（assistant config new，当前目录）"
    if [ "$(id -u)" = 0 ] && [ -n "${SUDO_USER:-}" ]; then
      sudo -u "$SUDO_USER" "$BINDIR/assistant" config new
    else
      "$BINDIR/assistant" config new
    fi
  fi

  echo
  echo "安装完成（macOS 测试形态，当前用户运行）："
  echo "  二进制: $BINDIR/assistant"
  echo "  配置:   $work_dir/config.json（空骨架；数据在 ./$work_dir/data）"
  echo
  print_next_steps "$BINDIR/assistant" "$work_dir"
}

# ============================================================
# Linux —— 服务形态：独立系统服务用户 + XDG 目录 + systemd
# ============================================================
install_linux() {
  # 非 root 时自动 sudo 重跑（与 Makefile 的 install 行为一致）
  if [ "$(id -u)" -ne 0 ]; then
    if [ -z "$SELF" ]; then
      # curl | bash：脚本在 stdin 上，先落地再提权重跑
      downloaded=$(mktemp /tmp/assistant-install-XXXXXX.sh)
      fetch "$RAW_SCRIPT_URL" -o "$downloaded"
      exec sudo bash "$downloaded" "$@"
    fi
    command -v sudo >/dev/null 2>&1 || die "需要 root 权限（当前非 root 且没有 sudo）"
    exec sudo bash "$SELF" "$@"
  fi

  local binary
  binary=$(resolve_binary)
  step "二进制: $binary"

  local service_group=$SERVICE_USER
  [ -n "$SERVICE_HOME" ] || SERVICE_HOME=/var/lib/$SERVICE_USER
  # 显式模式：工作目录即配置目录（config.json/daemon.json/data 全在这里），
  # systemd 单元以它为 WorkingDirectory；credentials.json 走平台标准配置目录
  # （服务用户的 ~/.config/Cosmic-Developers-Union/assistant/）
  local work_dir=$SERVICE_HOME/work

  # ---------- 独立系统服务用户（nologin、无密码，无需登陆） ----------
  local nologin=
  for candidate in /usr/sbin/nologin /sbin/nologin /bin/false; do
    [ -x "$candidate" ] && nologin=$candidate && break
  done
  [ -n "$nologin" ] || die "找不到 nologin（/usr/sbin/nologin、/sbin/nologin、/bin/false 都不存在）"

  if ! getent group "$service_group" >/dev/null 2>&1; then
    step "创建系统组: $service_group"
    groupadd --system "$service_group"
  fi
  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    step "服务用户已存在，跳过创建: $SERVICE_USER"
  else
    step "创建系统服务用户: $SERVICE_USER（home=$SERVICE_HOME，shell=$nologin，无密码）"
    useradd --system --gid "$service_group" --home-dir "$SERVICE_HOME" \
      --shell "$nologin" --comment "assistant service" "$SERVICE_USER"
  fi

  # ---------- 工作目录（配置 + 数据都在这里） ----------
  step "创建工作目录（属主 $SERVICE_USER，0700）：$work_dir"
  install -d -o "$SERVICE_USER" -g "$service_group" -m 0750 "$SERVICE_HOME"
  install -d -o "$SERVICE_USER" -g "$service_group" -m 0700 "$work_dir"

  # ---------- 安装二进制 ----------
  step "安装二进制到 $BINDIR/assistant"
  install -d "$BINDIR"
  install -m 0755 "$binary" "$BINDIR/assistant"

  # 以服务用户身份执行命令（runuser/sudo 都不经过登录 shell，nologin 不碍事）
  run_as_service_user() {
    local -a environment=(env "HOME=$SERVICE_HOME")
    if command -v runuser >/dev/null 2>&1; then
      runuser -u "$SERVICE_USER" -- "${environment[@]}" "$@"
    else
      sudo -u "$SERVICE_USER" "${environment[@]}" "$@"
    fi
  }

  # ---------- 空配置骨架（服务用户身份，落在工作目录） ----------
  if [ -f "$work_dir/config.json" ]; then
    step "配置已存在，跳过生成: $work_dir/config.json"
  else
    step "生成空配置骨架（assistant config new，以 $SERVICE_USER 身份）"
    run_as_service_user "$BINDIR/assistant" config new --config "$work_dir/config.json"
  fi

  # ---------- systemd 单元 ----------
  if [ "$INSTALL_SYSTEMD" = auto ]; then
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
      INSTALL_SYSTEMD=yes
    else
      INSTALL_SYSTEMD=no
    fi
  fi
  local unit_path=/etc/systemd/system/assistant.service
  if [ "$INSTALL_SYSTEMD" = yes ]; then
    # ProtectHome 与家目录位置冲突时（家目录在 /home 下）必须关闭
    local protect_home=true
    case "$SERVICE_HOME" in
      /home/*|/root|/root/*|/run/user/*) protect_home=false ;;
    esac
    step "写入 systemd 单元: $unit_path"
    cat > "$unit_path" <<EOF
# 由 assistant install.sh 生成；重新运行安装脚本即可更新
[Unit]
Description=assistant —— Gitea 仓库机器人与评审调度引擎
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$service_group
# 显式模式：配置与产物都在工作目录（./config.json 与 ./data）
WorkingDirectory=$work_dir
Environment=HOME=$SERVICE_HOME
ExecStart=$BINDIR/assistant run
Restart=on-failure
RestartSec=5s
# 加固：整盘只读 + 私有 /tmp，仅 XDG 目录可写；评审会话需要 claude 在 PATH
# （systemd 缺省 PATH 含 /usr/local/bin）或改在这里追加 Environment=PATH=
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=$protect_home
ReadWritePaths=$SERVICE_HOME
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    if [ "$ENABLE" = true ]; then
      step "systemctl enable assistant（开机自启；启动需配置完成后 systemctl start assistant）"
      systemctl enable assistant
    fi
  else
    step "跳过 systemd 单元（--systemd 可强制安装）"
  fi

  echo
  echo "安装完成："
  echo "  二进制:   $BINDIR/assistant"
  echo "  服务用户: $SERVICE_USER（系统用户，$nologin，无密码，无需登陆）"
  echo "  配置:     $work_dir/config.json（空骨架）"
  echo "  工作目录: $work_dir（配置与 data/ 运行树都在这里）"
  if [ "$INSTALL_SYSTEMD" = yes ]; then
    echo "  systemd:  $unit_path"
  fi
  echo
  echo "配置补全（凭据与配置都在工作目录）："
  echo "  1. token 最快路径：sudo -u $SERVICE_USER -H env GITEA_HOST=https://<gitea地址> GITEA_ACCESS_TOKEN=<token> $BINDIR/assistant list   # 只读验证"
  echo "  2. 长期配置：编辑 $work_dir/config.json（providers/channels/runtimes），"
  echo "     或 sudo -u $SERVICE_USER -H $BINDIR/assistant login <gitea地址> --user <管理员账号> 后 setup 建机器人账号"
  echo "  3. 校验: sudo -u $SERVICE_USER -H $BINDIR/assistant validate；claude CLI 装到 PATH（/usr/local/bin）"
  if [ "$INSTALL_SYSTEMD" = yes ]; then
    echo "  4. 启动: systemctl enable --now assistant；观测 journalctl -u assistant -f"
  else
    echo "  4. 前台跑: sudo -u $SERVICE_USER -H $BINDIR/assistant run"
  fi
}

case "$OS" in
  Darwin) install_darwin "$@" ;;
  Linux) install_linux "$@" ;;
  *) die "$OS 暂不支持自动安装（Windows 手动下载 $RELEASE_BASE ；或用 docker compose 部署）" ;;
esac
