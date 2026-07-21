#!/usr/bin/env bash
set -euo pipefail

# Meridian — Emby reverse proxy management panel
# Interactive installer / updater / uninstaller
# Usage: bash <(curl -sL https://raw.githubusercontent.com/snnabb/Meridian/master/install.sh)

REPO="snnabb/Meridian"
INSTALL_DIR="/usr/local/bin"
DATA_DIR="/opt/meridian"
SERVICE_FILE="/etc/systemd/system/meridian.service"
BIN_NAME="meridian"
SERVICE_USER="meridian"
SERVICE_GROUP="meridian"
INITIAL_SETUP_TOKEN=""

# ─── Colors ───
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail()  { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

download() {
    curl --proto '=https' --tlsv1.2 --retry 3 --retry-delay 2 -fsSL "$1" -o "$2"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        fail "缺少 sha256sum 或 shasum，无法校验下载文件"
    fi
}

generate_secret() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    else
        od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
    fi
}

ensure_service_user() {
    local nologin_shell
    nologin_shell=$(command -v nologin || true)
    nologin_shell=${nologin_shell:-/usr/sbin/nologin}
    if command -v useradd >/dev/null 2>&1; then
        if ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
            sudo groupadd --system "$SERVICE_GROUP"
        fi
        if ! id "$SERVICE_USER" >/dev/null 2>&1; then
            sudo useradd --system --gid "$SERVICE_GROUP" --home-dir "$DATA_DIR" --shell "$nologin_shell" --no-create-home "$SERVICE_USER"
        fi
    elif command -v adduser >/dev/null 2>&1; then
        if ! id "$SERVICE_USER" >/dev/null 2>&1; then
            sudo addgroup -S "$SERVICE_GROUP" 2>/dev/null || true
            sudo adduser -S -H -h "$DATA_DIR" -s "$nologin_shell" -G "$SERVICE_GROUP" "$SERVICE_USER"
        fi
    else
        fail "无法创建 systemd 服务用户：系统缺少 useradd/adduser"
    fi
}

# ─── Detect platform ───
detect_platform() {
    local os arch suffix
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)

    case "$os" in
        linux)  os="linux" ;;
        darwin) os="darwin" ;;
        *)      fail "不支持的操作系统: $os" ;;
    esac

    case "$arch" in
        x86_64|amd64)   arch="amd64" ;;
        aarch64|arm64)  arch="arm64" ;;
        *)              fail "不支持的架构: $arch" ;;
    esac

    suffix="${os}-${arch}"
    echo "$suffix"
}

# ─── Get latest version tag ───
get_latest_version() {
    curl --proto '=https' --tlsv1.2 --retry 3 -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//'
}

# ─── Get current installed version ───
get_current_version() {
    if command -v "$BIN_NAME" &>/dev/null; then
        "$BIN_NAME" --version 2>/dev/null || echo "已安装"
    else
        echo ""
    fi
}

# ─── Install / Update ───
do_install() {
    local suffix version asset url checksum_url tmp_dir binary_file checksum_file expected actual env_file

    info "检测平台..."
    suffix=$(detect_platform)
    ok "平台: $suffix"

    info "获取最新版本..."
    version=$(get_latest_version)
    if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
        fail "当前仓库还没有可用的 GitHub Release。请先从 Releases 页面下载，或改用 Docker / 源码构建。"
    fi
    ok "最新版本: $version"

    tmp_dir=$(mktemp -d)
    trap 'rm -rf -- "$tmp_dir"' EXIT
    asset="${BIN_NAME}-${suffix}"
    binary_file="${tmp_dir}/${asset}"
    checksum_file="${tmp_dir}/SHA256SUMS"
    url="https://github.com/${REPO}/releases/download/${version}/${asset}"
    checksum_url="https://github.com/${REPO}/releases/download/${version}/SHA256SUMS"
    info "下载 $url ..."
    download "$url" "$binary_file" || fail "二进制下载失败"
    download "$checksum_url" "$checksum_file" || fail "校验文件下载失败；为安全起见已停止安装"

    expected=$(awk -v file="$asset" '$2 == file { print $1; exit }' "$checksum_file")
    if ! printf '%s' "$expected" | grep -Eq '^[[:xdigit:]]{64}$'; then
        fail "SHA256SUMS 中缺少 ${asset} 的有效校验值"
    fi
    actual=$(sha256_file "$binary_file")
    expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
    actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
    if [ "$expected" != "$actual" ]; then
        fail "下载文件 SHA-256 校验失败"
    fi
    ok "SHA-256 校验通过"

    info "安装到 ${INSTALL_DIR}/${BIN_NAME} ..."
    sudo install -o root -g root -m 0755 "$binary_file" "${INSTALL_DIR}/${BIN_NAME}.new"
    sudo mv -f "${INSTALL_DIR}/${BIN_NAME}.new" "${INSTALL_DIR}/${BIN_NAME}"
    ok "二进制已安装"

    # Create data directory
    if [ -d /run/systemd/system ]; then
        ensure_service_user
        sudo install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0750 "$DATA_DIR"
        sudo chown -R "$SERVICE_USER:$SERVICE_GROUP" "$DATA_DIR"
    else
        sudo install -d -o "$(id -u)" -g "$(id -g)" -m 0750 "$DATA_DIR"
    fi
    ok "数据目录已准备: $DATA_DIR"

    # Generate JWT secret if not exists
    env_file="${DATA_DIR}/.env"
    if [ ! -f "$env_file" ]; then
        local secret env_tmp
        secret=$(generate_secret)
        INITIAL_SETUP_TOKEN=$(generate_secret)
        env_tmp="${tmp_dir}/meridian.env"
        printf 'JWT_SECRET=%s\nSETUP_TOKEN=%s\nPORT=9090\nDB_PATH=%s/meridian.db\n' \
            "$secret" "$INITIAL_SETUP_TOKEN" "$DATA_DIR" > "$env_tmp"
        if [ -d /run/systemd/system ]; then
            sudo install -o root -g "$SERVICE_GROUP" -m 0640 "$env_tmp" "$env_file"
        else
            sudo install -o "$(id -u)" -g "$(id -g)" -m 0600 "$env_tmp" "$env_file"
        fi
        ok "配置文件已生成: $env_file"
    else
        info "配置文件已存在，跳过: $env_file"
        if [ -d /run/systemd/system ]; then
            sudo chown root:"$SERVICE_GROUP" "$env_file"
            sudo chmod 0640 "$env_file"
        fi
    fi

    # Create systemd service
    if [ -d /run/systemd/system ]; then
        info "配置 systemd 服务..."
        local service_tmp="${tmp_dir}/meridian.service"
        cat > "$service_tmp" <<SVCEOF
[Unit]
Description=Meridian — Emby reverse proxy management panel
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
UMask=0077
EnvironmentFile=${DATA_DIR}/.env
ExecStart=${INSTALL_DIR}/${BIN_NAME}
WorkingDirectory=${DATA_DIR}
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectHostname=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictRealtime=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
ReadWritePaths=${DATA_DIR}
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SVCEOF
        sudo install -o root -g root -m 0644 "$service_tmp" "$SERVICE_FILE"
        sudo systemctl daemon-reload
        sudo systemctl enable meridian
        ok "systemd 服务已配置"

        echo ""
        read -rp "$(echo -e "${CYAN}是否立即启动 Meridian？[Y/n]:${NC} ")" start_now
        if [[ "$start_now" != "n" && "$start_now" != "N" ]]; then
            sudo systemctl restart meridian
            ok "Meridian 已启动"
        fi
    else
        warn "未检测到 systemd，跳过服务配置"
        echo -e "  手动启动: ${BOLD}set -a; source ${DATA_DIR}/.env; set +a; ${INSTALL_DIR}/${BIN_NAME}${NC}"
    fi

    echo ""
    echo -e "${GREEN}════════════════════════════════════════${NC}"
    echo -e "${GREEN}  Meridian $version 安装完成${NC}"
    echo -e "${GREEN}════════════════════════════════════════${NC}"
    echo -e "  面板地址:  ${BOLD}http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost'):9090${NC}"
    echo -e "  配置文件:  ${DATA_DIR}/.env"
    echo -e "  数据目录:  ${DATA_DIR}"
    echo -e "  服务管理:  systemctl {start|stop|restart|status} meridian"
    if [ -n "$INITIAL_SETUP_TOKEN" ]; then
        echo -e "  初始化令牌: ${BOLD}${INITIAL_SETUP_TOKEN}${NC}"
        echo -e "  ${YELLOW}请保存此令牌；首次创建管理员时需要。${NC}"
    fi
    echo ""

    rm -rf -- "$tmp_dir"
    trap - EXIT
}

# ─── Uninstall ───
do_uninstall() {
    echo ""
    warn "即将卸载 Meridian，以下内容将被移除："
    echo "  - ${INSTALL_DIR}/${BIN_NAME}"
    echo "  - ${SERVICE_FILE}"
    echo ""

    read -rp "$(echo -e "${RED}是否同时删除数据目录 ${DATA_DIR}？（含数据库和配置）[y/N]:${NC} ")" remove_data
    echo ""
    read -rp "$(echo -e "${RED}确认卸载？[y/N]:${NC} ")" confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
        info "已取消"
        exit 0
    fi

    # Stop service
    if [ -f "$SERVICE_FILE" ]; then
        sudo systemctl stop meridian 2>/dev/null || true
        sudo systemctl disable meridian 2>/dev/null || true
        sudo rm -f "$SERVICE_FILE"
        sudo systemctl daemon-reload
        ok "systemd 服务已移除"
    fi

    # Remove binary
    sudo rm -f -- "${INSTALL_DIR}/${BIN_NAME}"
    ok "二进制已移除"

    # Remove data
    if [[ "$remove_data" == "y" || "$remove_data" == "Y" ]]; then
        sudo rm -rf -- "$DATA_DIR"
        ok "数据目录已移除"
        if id "$SERVICE_USER" >/dev/null 2>&1 && command -v userdel >/dev/null 2>&1; then
            sudo userdel "$SERVICE_USER" 2>/dev/null || true
            ok "服务用户已移除"
        fi
    else
        info "数据目录已保留: $DATA_DIR"
    fi

    echo ""
    ok "Meridian 已卸载"
}

# ─── Main menu ───
main() {
    echo ""
    echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
    echo -e "${BOLD}║     Meridian 安装管理工具             ║${NC}"
    echo -e "${BOLD}║     Emby reverse proxy panel         ║${NC}"
    echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"
    echo ""

    local current
    current=$(get_current_version)
    if [ -n "$current" ]; then
        echo -e "  当前状态: ${GREEN}${current}${NC}"
    else
        echo -e "  当前状态: ${YELLOW}未安装${NC}"
    fi
    echo ""
    echo "  1) 安装 / 更新"
    echo "  2) 卸载"
    echo "  0) 退出"
    echo ""

    read -rp "请选择 [0-2]: " choice
    case "$choice" in
        1) do_install ;;
        2) do_uninstall ;;
        0) exit 0 ;;
        *) fail "无效选项" ;;
    esac
}

# Allow direct action via argument: install.sh install / uninstall
case "${1:-}" in
    install|update) do_install ;;
    uninstall|remove) do_uninstall ;;
    *) main ;;
esac
