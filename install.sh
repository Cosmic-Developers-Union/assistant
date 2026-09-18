#!/usr/bin/env bash
# install.sh —— assistant 主机部署（systemd 形态）：
#
#   1. 创建独立系统服务用户（nologin、无密码，仅供服务运行使用，无需登陆）；
#   2. 按用户家目录建立 XDG 标准目录（config/data/state/cache，落点与
#      assistant 二进制的缺省解析一致）；
#   3. 安装二进制到 <prefix>/bin/assistant（来源优先级：--bin > 检出内现成的
#      ./assistant > go 构建 > GitHub Release 下载）；
#   4. 以服务用户身份生成空配置骨架（assistant config new）；
#   5. 安装 systemd 单元（不自动启动：配置完成前 run 无法运行）。
#
# 支持 curl -fsSL https://raw.githubusercontent.com/Cosmic-Developers-Union/assistant/main/install.sh | bash
# 一键执行（stdin 模式自动下载 Release 二进制，非 root 自动提权）。
# 幂等：重复执行只更新二进制与单元文件，已存在的用户/目录/配置不动。
# 用法见 --help。
set -euo pipefail

GITHUB_REPO=Cosmic-Developers-Union/assistant
RAW_SCRIPT_URL=https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh
RELEASE_BASE=https://github.com/$GITHUB_REPO/releases/latest/download

usage() {
  cat <<'EOF'
用法: install.sh [选项]
      curl -fsSL https://raw.githubusercontent.com/Cosmic-Developers-Union/assistant/main/install.sh | bash

主机部署 assistant：独立系统服务用户 + XDG 目录 + 二进制 + systemd 单元 + 空配置。
二进制来源优先级：--bin 指定 > 检出内现成的 ./assistant > go 构建 > GitHub Release 下载。

选项:
  --user NAME      服务用户名（缺省 assistant；组名同名）
  --home PATH      服务用户家目录（缺省 /var/lib/<user>；XDG 目录建在其下）
  --bin PATH       使用已构建的二进制（跳过构建/下载）
  --prefix PATH    二进制安装前缀（缺省 /usr/local，装到 <prefix>/bin/assistant）
  --systemd        强制安装 systemd 单元
  --no-systemd     不安装 systemd 单元（缺省 auto：检测到运行中的 systemd 才装）
  --enable         装完后 systemctl enable（开机自启；启动仍需配置完成后手动）
  -h, --help       显示本帮助

示例:
  curl -fsSL https://raw.githubusercontent.com/Cosmic-Developers-Union/assistant/main/install.sh | bash
  sudo ./install.sh --enable                 # 检出内安装 + 设开机自启
  sudo ./install.sh --user bot --home /srv/bot --bin ./assistant --prefix /opt/assistant
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

SERVICE_GROUP=$SERVICE_USER
[ -n "$SERVICE_HOME" ] || SERVICE_HOME=/var/lib/$SERVICE_USER
BINDIR=$PREFIX/bin
CONFIG_DIR=$SERVICE_HOME/.config/Cosmic-Developers-Union/assistant
DATA_DIR=$SERVICE_HOME/.local/share/Cosmic-Developers-Union/assistant
STATE_DIR=$SERVICE_HOME/.local/state/Cosmic-Developers-Union/assistant
CACHE_DIR=$SERVICE_HOME/.cache/Cosmic-Developers-Union/assistant

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

# 脚本路径与模式已在参数解析前确定；非 root 时自动 sudo 重跑（与 Makefile 一致）
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

# 本脚本做的是 Linux 主机部署（useradd/nologin/systemd）；其它系统直接拒绝，
# 避免创建出无用的用户与目录
case "$(uname -s)" in
  Linux) ;;
  *) die "install.sh 面向 Linux 主机部署（需要 useradd/systemd），当前系统 $(uname -s)。
macOS/Windows 请手动下载二进制: https://github.com/$GITHUB_REPO/releases ，或用 docker compose 部署" ;;
esac

# ---------- 1. 二进制 ----------
download_release() {
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "不支持的架构: $(uname -m)（用 --bin 指定二进制，或在检出内运行本脚本）" ;;
  esac
  url="$RELEASE_BASE/assistant-linux-$arch"
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
      step "未找到预构建二进制，用 go 构建（Linux/$(uname -m)）"
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
BINARY=$(resolve_binary)
step "二进制: $BINARY"

# ---------- 2. 独立系统服务用户（nologin、无密码，无需登陆） ----------
NOLOGIN=
for candidate in /usr/sbin/nologin /sbin/nologin /bin/false; do
  [ -x "$candidate" ] && NOLOGIN=$candidate && break
done
[ -n "$NOLOGIN" ] || die "找不到 nologin（/usr/sbin/nologin、/sbin/nologin、/bin/false 都不存在）"

if ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
  step "创建系统组: $SERVICE_GROUP"
  groupadd --system "$SERVICE_GROUP"
fi
if id -u "$SERVICE_USER" >/dev/null 2>&1; then
  step "服务用户已存在，跳过创建: $SERVICE_USER"
else
  step "创建系统服务用户: $SERVICE_USER（home=$SERVICE_HOME，shell=$NOLOGIN，无密码）"
  useradd --system --gid "$SERVICE_GROUP" --home-dir "$SERVICE_HOME" \
    --shell "$NOLOGIN" --comment "assistant service" "$SERVICE_USER"
fi

# ---------- 3. XDG 标准目录 ----------
# 与二进制缺省解析一致：XDG_CONFIG_HOME ~/.config、XDG_DATA_HOME ~/.local/share、
# XDG_STATE_HOME ~/.local/state、XDG_CACHE_HOME ~/.cache，加命名空间段。
step "创建 XDG 目录（属主 $SERVICE_USER，配置/数据/状态 0700）"
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0750 "$SERVICE_HOME"
for dir in "$CONFIG_DIR" "$DATA_DIR" "$STATE_DIR" "$CACHE_DIR"; do
  install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0700 "$dir"
done

# ---------- 4. 安装二进制 ----------
step "安装二进制到 $BINDIR/assistant"
install -d "$BINDIR"
install -m 0755 "$BINARY" "$BINDIR/assistant"

# 以服务用户身份执行命令（runuser/sudo 都不经过登录 shell，nologin 不碍事）
run_as_service_user() {
  local -a environment=(
    env
    "HOME=$SERVICE_HOME"
    "XDG_CONFIG_HOME=$SERVICE_HOME/.config"
    "XDG_DATA_HOME=$SERVICE_HOME/.local/share"
    "XDG_STATE_HOME=$SERVICE_HOME/.local/state"
    "XDG_CACHE_HOME=$SERVICE_HOME/.cache"
  )
  if command -v runuser >/dev/null 2>&1; then
    runuser -u "$SERVICE_USER" -- "${environment[@]}" "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo -u "$SERVICE_USER" "${environment[@]}" "$@"
  else
    die "需要 runuser 或 sudo 以服务用户身份执行: $*"
  fi
}

# ---------- 5. 空配置骨架（服务用户身份，落 XDG 配置目录） ----------
if [ -f "$CONFIG_DIR/config.json" ]; then
  step "配置已存在，跳过生成: $CONFIG_DIR/config.json"
else
  step "生成空配置骨架（assistant config new，以 $SERVICE_USER 身份）"
  run_as_service_user "$BINDIR/assistant" config new
fi

# ---------- 6. systemd 单元 ----------
if [ "$INSTALL_SYSTEMD" = auto ]; then
  if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    INSTALL_SYSTEMD=yes
  else
    INSTALL_SYSTEMD=no
  fi
fi
UNIT_PATH=/etc/systemd/system/assistant.service
if [ "$INSTALL_SYSTEMD" = yes ]; then
  # ProtectHome 与家目录位置冲突时（家目录在 /home 下）必须关闭
  protect_home=true
  case "$SERVICE_HOME" in
    /home/*|/root|/root/*|/run/user/*) protect_home=false ;;
  esac
  step "写入 systemd 单元: $UNIT_PATH"
  cat > "$UNIT_PATH" <<EOF
# 由 assistant install.sh 生成；重新运行安装脚本即可更新
[Unit]
Description=assistant —— Gitea 仓库机器人与评审调度引擎
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_GROUP
WorkingDirectory=$SERVICE_HOME
Environment=HOME=$SERVICE_HOME
Environment=XDG_CONFIG_HOME=$SERVICE_HOME/.config
Environment=XDG_DATA_HOME=$SERVICE_HOME/.local/share
Environment=XDG_STATE_HOME=$SERVICE_HOME/.local/state
Environment=XDG_CACHE_HOME=$SERVICE_HOME/.cache
ExecStart=$BINDIR/assistant run
Restart=on-failure
RestartSec=5s
# 加固：整盘只读 + 私有 /tmp，仅 XDG 目录可写；评审会话需要 claude 在 PATH
# （systemd 缺省 PATH 含 /usr/local/bin）或改在这里追加 Environment=PATH=
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=$protect_home
ReadWritePaths=$CONFIG_DIR $DATA_DIR $STATE_DIR $CACHE_DIR
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

# ---------- 完成 ----------
echo
echo "安装完成："
echo "  二进制:   $BINDIR/assistant"
echo "  服务用户: $SERVICE_USER（系统用户，$NOLOGIN，无密码，无需登陆）"
echo "  配置:     $CONFIG_DIR/config.json（空骨架，instances 为空）"
echo "  数据:     $DATA_DIR"
echo "  状态:     $STATE_DIR"
if [ "$INSTALL_SYSTEMD" = yes ]; then
  echo "  systemd:  $UNIT_PATH"
fi
echo
echo "下一步（以服务用户身份操作；凭据与配置都随 XDG 配置目录走）："
echo "  1. 补全配置：sudo -u $SERVICE_USER -H $BINDIR/assistant login <gitea地址> --user <管理员账号>"
echo "     然后 sudo -u $SERVICE_USER -H $BINDIR/assistant setup 建 ai/merge 机器人账号；"
echo "     或直接编辑 $CONFIG_DIR/config.json（providers/instances）"
echo "  2. 校验：   sudo -u $SERVICE_USER -H $BINDIR/assistant validate"
echo "  3. 评审会话需要 claude CLI 在 PATH（建议装到 /usr/local/bin）"
if [ "$INSTALL_SYSTEMD" = yes ]; then
  echo "  4. 启动：   systemctl enable --now assistant；观测 journalctl -u assistant -f"
fi
exit 0
