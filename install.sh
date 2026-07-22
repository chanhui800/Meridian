#!/usr/bin/env bash
set -euo pipefail

# Meridian — Emby reverse proxy management panel
# Installer / updater / service management tool
# Usage: bash <(curl -fsSL https://raw.githubusercontent.com/snnabb/Meridian/master/install.sh)

REPO="${MERIDIAN_REPO:-snnabb/Meridian}"
INSTALL_DIR="${MERIDIAN_INSTALL_DIR:-/usr/local/bin}"
DATA_DIR="${MERIDIAN_DATA_DIR:-/opt/meridian}"
BACKUP_DIR="${MERIDIAN_BACKUP_DIR:-/opt/meridian-backups}"
SERVICE_FILE="${MERIDIAN_SERVICE_FILE:-/etc/systemd/system/meridian.service}"
SERVICE_NAME="${MERIDIAN_SERVICE_NAME:-meridian}"
BIN_NAME="meridian"
SERVICE_USER="meridian"
SERVICE_GROUP="meridian"
ROOT_GROUP="${MERIDIAN_ROOT_GROUP:-$(id -gn 0 2>/dev/null || printf 'root')}"

while [ "$INSTALL_DIR" != "/" ] && [[ "$INSTALL_DIR" == */ ]]; do INSTALL_DIR="${INSTALL_DIR%/}"; done
while [ "$DATA_DIR" != "/" ] && [[ "$DATA_DIR" == */ ]]; do DATA_DIR="${DATA_DIR%/}"; done
while [ "$BACKUP_DIR" != "/" ] && [[ "$BACKUP_DIR" == */ ]]; do BACKUP_DIR="${BACKUP_DIR%/}"; done

PREVIOUS_BIN="${INSTALL_DIR}/${BIN_NAME}.previous"

INITIAL_SETUP_TOKEN=""
REQUESTED_VERSION="${MERIDIAN_VERSION:-}"
ASSUME_YES="${MERIDIAN_ASSUME_YES:-0}"
PURGE_DATA=0
FOLLOW_LOGS=0
LOG_LINES=100
LAST_BACKUP_PATH=""
ROOT_PREFIX=()
INSTALL_TMP_DIR=""
INSTALL_RESTART_SERVICE=0
INSTALL_BINARY_REPLACED=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info() { echo -e "${CYAN}[INFO]${NC} $*"; }
ok() { echo -e "${GREEN}[OK]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail() { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || fail "缺少必要命令: $1"
}

init_privilege() {
    if [ "${EUID}" -eq 0 ]; then
        ROOT_PREFIX=()
        return
    fi
    need_cmd sudo
    sudo -v
    ROOT_PREFIX=(sudo)
}

as_root() {
    "${ROOT_PREFIX[@]}" "$@"
}

is_systemd() {
    [ "$(uname -s)" = "Linux" ] && [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1
}

service_is_active() {
    is_systemd && systemctl is-active --quiet "$SERVICE_NAME"
}

valid_version() {
    [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]
}

valid_log_lines() {
    [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 5000 ]
}

validate_data_dir() {
    case "$DATA_DIR" in
        ""|/|/bin|/boot|/dev|/etc|/home|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/var)
            fail "拒绝对不安全的数据目录执行操作: ${DATA_DIR:-<empty>}"
            ;;
        *//*|*/../*|*/..|*/./*|*/.|*$'\n'*)
            fail "数据目录包含不安全的路径片段: $DATA_DIR"
            ;;
    esac
    [[ "$DATA_DIR" = /* ]] || fail "数据目录必须是绝对路径: $DATA_DIR"
}

validate_backup_dir() {
    case "$BACKUP_DIR" in
        ""|/|/bin|/boot|/dev|/etc|/home|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/var)
            fail "拒绝使用不安全的备份目录: ${BACKUP_DIR:-<empty>}"
            ;;
        *//*|*/../*|*/..|*/./*|*/.|*$'\n'*)
            fail "备份目录包含不安全的路径片段: $BACKUP_DIR"
            ;;
    esac
    [[ "$BACKUP_DIR" = /* ]] || fail "备份目录必须是绝对路径: $BACKUP_DIR"
}

ask_yes_no() {
    local prompt="$1" default_yes="${2:-0}" answer
    if [ "$ASSUME_YES" = "1" ]; then
        return 0
    fi
    if [ "$default_yes" = "1" ]; then
        read -rp "$(echo -e "${CYAN}${prompt} [Y/n]:${NC} ")" answer
        [[ "$answer" != "n" && "$answer" != "N" ]]
    else
        read -rp "$(echo -e "${CYAN}${prompt} [y/N]:${NC} ")" answer
        [[ "$answer" = "y" || "$answer" = "Y" ]]
    fi
}

download() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --retry 3 --retry-delay 2 --connect-timeout 15 -fsSL "$1" -o "$2"
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

detect_platform() {
    local os arch
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)

    case "$os" in
        linux) os="linux" ;;
        darwin) os="darwin" ;;
        *) fail "不支持的操作系统: $os" ;;
    esac

    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) fail "不支持的架构: $arch" ;;
    esac

    printf '%s-%s\n' "$os" "$arch"
}

get_latest_version() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 \
        --connect-timeout 15 -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//'
}

resolve_version() {
    local version="$REQUESTED_VERSION"
    if [ -z "$version" ]; then
        info "获取最新版本..." >&2
        version=$(get_latest_version) || true
    fi
    valid_version "$version" || fail "版本格式无效或仓库尚无可用 Release: ${version:-<empty>}"
    printf '%s\n' "$version"
}

get_current_version() {
    if [ -x "${INSTALL_DIR}/${BIN_NAME}" ]; then
        "${INSTALL_DIR}/${BIN_NAME}" --version 2>/dev/null || echo "已安装（版本未知）"
    else
        echo ""
    fi
}

read_config_port() {
    local env_file="${DATA_DIR}/.env" port="9090"
    local configured=""
    # $1 is an awk field reference, not a shell variable.
    # shellcheck disable=SC2016
    local port_program='$1 == "PORT" { sub(/^[^=]*=/, ""); print; exit }'
    if [ -r "$env_file" ]; then
        configured=$(awk -F= "$port_program" "$env_file")
    elif [ "${#ROOT_PREFIX[@]}" -gt 0 ]; then
        configured=$(as_root awk -F= "$port_program" "$env_file" 2>/dev/null || true)
    elif command -v sudo >/dev/null 2>&1 && sudo -n test -r "$env_file" 2>/dev/null; then
        configured=$(sudo -n awk -F= "$port_program" "$env_file" 2>/dev/null || true)
    fi
    if [[ "$configured" =~ ^[0-9]+$ ]] && [ "$configured" -ge 1 ] && [ "$configured" -le 65535 ]; then
        port="$configured"
    fi
    printf '%s\n' "$port"
}

health_url() {
    printf 'http://127.0.0.1:%s/api/auth/check\n' "$(read_config_port)"
}

wait_for_health() {
    local attempts="${1:-20}" url code i
    command -v curl >/dev/null 2>&1 || return 1
    url=$(health_url)
    for ((i = 1; i <= attempts; i++)); do
        code=$(curl --noproxy '*' --proto '=http' --connect-timeout 1 --max-time 2 \
            -sS -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)
        if [ "$code" = "200" ]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

ensure_service_user() {
    local nologin_shell
    nologin_shell=$(command -v nologin || true)
    nologin_shell=${nologin_shell:-/usr/sbin/nologin}

    if command -v useradd >/dev/null 2>&1; then
        if ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
            as_root groupadd --system "$SERVICE_GROUP"
        fi
        if ! id "$SERVICE_USER" >/dev/null 2>&1; then
            as_root useradd --system --gid "$SERVICE_GROUP" --home-dir "$DATA_DIR" \
                --shell "$nologin_shell" --no-create-home "$SERVICE_USER"
        fi
    elif command -v adduser >/dev/null 2>&1; then
        if ! id "$SERVICE_USER" >/dev/null 2>&1; then
            as_root addgroup -S "$SERVICE_GROUP" 2>/dev/null || true
            as_root adduser -S -H -h "$DATA_DIR" -s "$nologin_shell" -G "$SERVICE_GROUP" "$SERVICE_USER"
        fi
    else
        fail "无法创建 systemd 服务用户：系统缺少 useradd/adduser"
    fi
}

prepare_data_and_config() {
    local tmp_dir="$1" env_file="${DATA_DIR}/.env"
    validate_data_dir

    if is_systemd; then
        ensure_service_user
        as_root install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0750 "$DATA_DIR"
        as_root chown -R "$SERVICE_USER:$SERVICE_GROUP" "$DATA_DIR"
    else
        as_root install -d -o "$(id -u)" -g "$(id -g)" -m 0750 "$DATA_DIR"
    fi
    ok "数据目录已准备: $DATA_DIR"

    if [ ! -f "$env_file" ]; then
        local secret env_tmp
        secret=$(generate_secret)
        INITIAL_SETUP_TOKEN=$(generate_secret)
        env_tmp="${tmp_dir}/meridian.env"
        printf 'JWT_SECRET=%s\nSETUP_TOKEN=%s\nPORT=9090\nDB_PATH=%s/meridian.db\n' \
            "$secret" "$INITIAL_SETUP_TOKEN" "$DATA_DIR" > "$env_tmp"
        if is_systemd; then
            as_root install -o root -g "$SERVICE_GROUP" -m 0640 "$env_tmp" "$env_file"
        else
            as_root install -o "$(id -u)" -g "$(id -g)" -m 0600 "$env_tmp" "$env_file"
        fi
        ok "配置文件已生成: $env_file"
    else
        info "配置文件已存在并将保留: $env_file"
        if is_systemd; then
            as_root chown root:"$SERVICE_GROUP" "$env_file"
            as_root chmod 0640 "$env_file"
        fi
    fi
}

write_systemd_service() {
    local tmp_dir="$1"
    local service_tmp="${tmp_dir}/meridian.service"
    is_systemd || return 0

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
    as_root install -o root -g root -m 0644 "$service_tmp" "$SERVICE_FILE"
    as_root systemctl daemon-reload
    as_root systemctl enable "$SERVICE_NAME" >/dev/null
    ok "systemd 服务已配置并启用"
}

create_backup_archive() {
    local label="${1:-manual}" stamp safe_label archive archive_tmp data_parent data_base
    validate_data_dir
    validate_backup_dir
    [ -d "$DATA_DIR" ] || return 1
    need_cmd tar
    need_cmd date

    safe_label=$(printf '%s' "$label" | tr -cd 'A-Za-z0-9._-')
    [ -n "$safe_label" ] || safe_label="manual"
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    archive="${BACKUP_DIR}/${BIN_NAME}-${safe_label}-${stamp}-$$.tar.gz"
    archive_tmp="${archive}.tmp.$$"
    data_parent=$(dirname -- "$DATA_DIR")
    data_base=$(basename -- "$DATA_DIR")

    as_root install -d -o root -g "$ROOT_GROUP" -m 0700 "$BACKUP_DIR"
    if ! as_root tar -C "$data_parent" -czf "$archive_tmp" "$data_base"; then
        as_root rm -f -- "$archive_tmp"
        return 1
    fi
    as_root chmod 0600 "$archive_tmp"
    as_root mv -f "$archive_tmp" "$archive"
    LAST_BACKUP_PATH="$archive"
    return 0
}

restore_previous_binary() {
    [ -f "$PREVIOUS_BIN" ] || return 1
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$PREVIOUS_BIN" "${INSTALL_DIR}/${BIN_NAME}.rollback"
    as_root mv -f "${INSTALL_DIR}/${BIN_NAME}.rollback" "${INSTALL_DIR}/${BIN_NAME}"
}

cleanup_install() {
    local exit_code=$?
    if [ "$exit_code" -ne 0 ] && [ "$INSTALL_BINARY_REPLACED" = "1" ] && [ -f "$PREVIOUS_BIN" ]; then
        warn "安装中断，正在恢复上一版本二进制..."
        restore_previous_binary || true
    fi
    if [ "$INSTALL_RESTART_SERVICE" = "1" ] && is_systemd; then
        as_root systemctl start "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
    if [ -n "$INSTALL_TMP_DIR" ] && [ -d "$INSTALL_TMP_DIR" ] && [ "$INSTALL_TMP_DIR" != "/" ]; then
        rm -rf -- "$INSTALL_TMP_DIR"
    fi
    return "$exit_code"
}

do_install() {
    local suffix version asset url checksum_url binary_file checksum_file expected actual
    local current_binary="${INSTALL_DIR}/${BIN_NAME}" current_version was_active=0 should_start=0

    need_cmd curl
    need_cmd awk
    need_cmd grep
    need_cmd install
    need_cmd mktemp
    need_cmd sed
    need_cmd tr
    init_privilege

    info "检测平台..."
    suffix=$(detect_platform)
    ok "平台: $suffix"
    version=$(resolve_version)
    ok "目标版本: $version"

    INSTALL_TMP_DIR=$(mktemp -d)
    trap cleanup_install EXIT
    asset="${BIN_NAME}-${suffix}"
    binary_file="${INSTALL_TMP_DIR}/${asset}"
    checksum_file="${INSTALL_TMP_DIR}/SHA256SUMS"
    url="https://github.com/${REPO}/releases/download/${version}/${asset}"
    checksum_url="https://github.com/${REPO}/releases/download/${version}/SHA256SUMS"

    info "下载 $url ..."
    download "$url" "$binary_file" || fail "二进制下载失败；请检查版本、平台和网络"
    download "$checksum_url" "$checksum_file" || fail "校验文件下载失败；为安全起见已停止安装"

    expected=$(awk -v file="$asset" '$2 == file || $2 == "*" file { print $1; exit }' "$checksum_file")
    if ! printf '%s' "$expected" | grep -Eq '^[[:xdigit:]]{64}$'; then
        fail "SHA256SUMS 中缺少 ${asset} 的有效校验值"
    fi
    actual=$(sha256_file "$binary_file")
    expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
    actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
    [ "$expected" = "$actual" ] || fail "下载文件 SHA-256 校验失败"
    ok "SHA-256 校验通过"

    current_version=$(get_current_version)
    if service_is_active; then
        was_active=1
    fi

    if [ -x "$current_binary" ] && is_systemd && [ -d "$DATA_DIR" ]; then
        if [ "$was_active" = "1" ]; then
            info "暂停服务以创建一致性升级备份..."
            as_root systemctl stop "$SERVICE_NAME"
            INSTALL_RESTART_SERVICE=1
        fi
        if create_backup_archive "pre-${version}"; then
            ok "升级前备份已创建: $LAST_BACKUP_PATH"
        else
            fail "升级前备份失败，未替换现有程序"
        fi
    elif [ -x "$current_binary" ] && ! is_systemd; then
        warn "未检测到 systemd，无法确认手动进程是否已停止；更新前请自行备份 ${DATA_DIR}"
    fi

    prepare_data_and_config "$INSTALL_TMP_DIR"
    write_systemd_service "$INSTALL_TMP_DIR"

    as_root install -d -o root -g "$ROOT_GROUP" -m 0755 "$INSTALL_DIR"
    if [ -x "$current_binary" ]; then
        as_root install -o root -g "$ROOT_GROUP" -m 0755 "$current_binary" "${PREVIOUS_BIN}.new"
        as_root mv -f "${PREVIOUS_BIN}.new" "$PREVIOUS_BIN"
        ok "上一版本二进制已保留: $PREVIOUS_BIN"
    fi
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$binary_file" "${current_binary}.new"
    as_root mv -f "${current_binary}.new" "$current_binary"
    INSTALL_BINARY_REPLACED=1
    ok "二进制已安装: $current_binary"

    if is_systemd; then
        if [ "$was_active" = "1" ]; then
            should_start=1
        elif ask_yes_no "是否立即启动 Meridian？" 1; then
            should_start=1
        fi

        if [ "$should_start" = "1" ]; then
            info "启动服务并执行健康检查..."
            as_root systemctl restart "$SERVICE_NAME"
            INSTALL_RESTART_SERVICE=0
            if wait_for_health 20; then
                ok "服务健康检查通过"
            elif [ -f "$PREVIOUS_BIN" ]; then
                warn "新版本未通过健康检查，正在自动回滚..."
                restore_previous_binary
                INSTALL_BINARY_REPLACED=0
                as_root systemctl restart "$SERVICE_NAME"
                if wait_for_health 20; then
                    fail "新版本启动失败，已恢复并启动上一版本；请查看日志"
                fi
                fail "新版本启动失败，上一版本也未通过健康检查；请运行日志命令排查"
            else
                fail "服务未通过健康检查；请运行日志命令排查"
            fi
        fi
    else
        warn "未检测到 systemd，跳过服务配置与健康检查"
        echo -e "  手动启动: ${BOLD}set -a; source ${DATA_DIR}/.env; set +a; ${current_binary}${NC}"
    fi

    INSTALL_BINARY_REPLACED=0
    INSTALL_RESTART_SERVICE=0
    rm -rf -- "$INSTALL_TMP_DIR"
    INSTALL_TMP_DIR=""
    trap - EXIT

    echo ""
    echo -e "${GREEN}════════════════════════════════════════${NC}"
    echo -e "${GREEN}  Meridian $version 安装完成${NC}"
    echo -e "${GREEN}════════════════════════════════════════${NC}"
    if [ -n "$current_version" ]; then
        echo -e "  升级前版本:  ${current_version}"
    fi
    echo -e "  面板地址:    ${BOLD}http://127.0.0.1:$(read_config_port)${NC}"
    echo -e "  配置文件:    ${DATA_DIR}/.env"
    echo -e "  数据目录:    ${DATA_DIR}"
    echo -e "  状态检查:    bash install.sh status"
    echo -e "  查看日志:    bash install.sh logs"
    echo -e "  创建备份:    bash install.sh backup"
    if [ -n "$INITIAL_SETUP_TOKEN" ]; then
        echo -e "  初始化令牌:  ${BOLD}${INITIAL_SETUP_TOKEN}${NC}"
        echo -e "  ${YELLOW}请立即保存此令牌；首次创建管理员时需要。${NC}"
    fi
    echo ""
}

do_status() {
    local current latest state enabled url code
    current=$(get_current_version)
    latest=""
    if command -v curl >/dev/null 2>&1; then
        latest=$(get_latest_version || true)
    fi

    echo ""
    echo -e "${BOLD}Meridian 状态${NC}"
    echo "  已安装版本: ${current:-未安装}"
    echo "  最新版本:   ${latest:-无法获取}"
    echo "  二进制:     ${INSTALL_DIR}/${BIN_NAME}"
    echo "  数据目录:   ${DATA_DIR}"

    if is_systemd; then
        state=$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)
        enabled=$(systemctl is-enabled "$SERVICE_NAME" 2>/dev/null || true)
        echo "  服务状态:   ${state:-unknown}"
        echo "  开机启动:   ${enabled:-unknown}"
    else
        echo "  服务状态:   未使用 systemd 管理"
    fi

    url=$(health_url)
    code=""
    if command -v curl >/dev/null 2>&1; then
        code=$(curl --noproxy '*' --proto '=http' --connect-timeout 1 --max-time 2 \
            -sS -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)
    fi
    if [ "$code" = "200" ]; then
        echo -e "  健康检查:   ${GREEN}正常（HTTP 200）${NC}"
    else
        echo -e "  健康检查:   ${YELLOW}不可用${NC}"
    fi
    echo "  检查地址:   $url"
    echo ""
}

do_restart() {
    is_systemd || fail "当前系统未使用 systemd 管理 Meridian"
    init_privilege
    [ -f "$SERVICE_FILE" ] || fail "尚未安装 Meridian systemd 服务"
    info "重启 Meridian..."
    as_root systemctl restart "$SERVICE_NAME"
    if wait_for_health 20; then
        ok "Meridian 已重启，健康检查通过"
    else
        as_root systemctl status "$SERVICE_NAME" --no-pager || true
        fail "服务重启后未通过健康检查"
    fi
}

do_logs() {
    is_systemd || fail "当前系统未使用 systemd，无法读取 journal 日志"
    valid_log_lines "$LOG_LINES" || fail "日志行数必须是 1-5000 的整数"
    init_privilege
    if [ "$FOLLOW_LOGS" = "1" ]; then
        as_root journalctl -u "$SERVICE_NAME" -n "$LOG_LINES" -f
    else
        as_root journalctl -u "$SERVICE_NAME" -n "$LOG_LINES" --no-pager
    fi
}

do_backup() {
    local was_active=0 backup_ok=0
    init_privilege
    need_cmd tar
    need_cmd date
    validate_data_dir
    [ -d "$DATA_DIR" ] || fail "数据目录不存在: $DATA_DIR"

    if service_is_active; then
        was_active=1
        info "短暂停止服务以创建一致性备份..."
        as_root systemctl stop "$SERVICE_NAME"
    elif ! is_systemd; then
        warn "未检测到 systemd；请确认没有手动运行的 Meridian 进程"
    fi

    if create_backup_archive "manual"; then
        backup_ok=1
    fi

    if [ "$was_active" = "1" ]; then
        as_root systemctl start "$SERVICE_NAME"
        if ! wait_for_health 20; then
            fail "备份已创建，但服务重新启动后未通过健康检查: $LAST_BACKUP_PATH"
        fi
    fi

    [ "$backup_ok" = "1" ] || fail "备份失败"
    ok "备份已创建: $LAST_BACKUP_PATH"
    warn "备份包含数据库、JWT 密钥和初始化配置，请妥善保管"
}

do_rollback() {
    local current_binary="${INSTALL_DIR}/${BIN_NAME}" tmp_dir was_active=0
    is_systemd || fail "自动回滚仅支持由 systemd 管理的安装"
    [ -x "$current_binary" ] || fail "当前二进制不存在: $current_binary"
    [ -f "$PREVIOUS_BIN" ] || fail "没有可回滚的上一版本: $PREVIOUS_BIN"
    init_privilege
    need_cmd tar
    need_cmd date

    tmp_dir=$(mktemp -d)
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$current_binary" "${tmp_dir}/${BIN_NAME}.current"
    if service_is_active; then
        was_active=1
        as_root systemctl stop "$SERVICE_NAME"
    fi

    if [ -d "$DATA_DIR" ]; then
        if create_backup_archive "pre-rollback"; then
            ok "回滚前数据备份已创建: $LAST_BACKUP_PATH"
        else
            [ "$was_active" = "1" ] && as_root systemctl start "$SERVICE_NAME"
            rm -rf -- "$tmp_dir"
            fail "回滚前备份失败，未更改当前版本"
        fi
    fi

    restore_previous_binary || fail "恢复上一版本二进制失败"
    if [ "$was_active" = "1" ]; then
        as_root systemctl start "$SERVICE_NAME"
        if ! wait_for_health 20; then
            warn "上一版本未通过健康检查，恢复回滚前版本..."
            as_root install -o root -g "$ROOT_GROUP" -m 0755 "${tmp_dir}/${BIN_NAME}.current" "${current_binary}.new"
            as_root mv -f "${current_binary}.new" "$current_binary"
            as_root systemctl restart "$SERVICE_NAME"
            rm -rf -- "$tmp_dir"
            fail "回滚失败，已恢复回滚前二进制"
        fi
    fi

    as_root install -o root -g "$ROOT_GROUP" -m 0755 "${tmp_dir}/${BIN_NAME}.current" "${PREVIOUS_BIN}.new"
    as_root mv -f "${PREVIOUS_BIN}.new" "$PREVIOUS_BIN"
    rm -rf -- "$tmp_dir"
    ok "已回滚到上一版本；再次执行 rollback 可切回刚才的版本"
}

do_uninstall() {
    local remove_data="$PURGE_DATA"
    init_privilege
    echo ""
    warn "即将卸载 Meridian 程序与 systemd 服务"

    if [ "$ASSUME_YES" != "1" ]; then
        if ask_yes_no "是否同时删除数据目录 ${DATA_DIR}？（含数据库和密钥）" 0; then
            remove_data=1
        fi
        ask_yes_no "确认卸载？" 0 || { info "已取消"; return 0; }
    fi

    if is_systemd && [ -f "$SERVICE_FILE" ]; then
        as_root systemctl stop "$SERVICE_NAME" 2>/dev/null || true
        as_root systemctl disable "$SERVICE_NAME" 2>/dev/null || true
        as_root rm -f -- "$SERVICE_FILE"
        as_root systemctl daemon-reload
        ok "systemd 服务已移除"
    fi

    as_root rm -f -- "${INSTALL_DIR}/${BIN_NAME}" "$PREVIOUS_BIN" \
        "${INSTALL_DIR}/${BIN_NAME}.new" "${INSTALL_DIR}/${BIN_NAME}.rollback"
    ok "二进制已移除"

    if [ "$remove_data" = "1" ]; then
        validate_data_dir
        as_root rm -rf -- "$DATA_DIR"
        ok "数据目录已移除"
        if id "$SERVICE_USER" >/dev/null 2>&1 && command -v userdel >/dev/null 2>&1; then
            as_root userdel "$SERVICE_USER" 2>/dev/null || true
            ok "服务用户已移除"
        fi
    else
        info "数据目录已保留: $DATA_DIR"
    fi

    ok "Meridian 已卸载"
}

usage() {
    cat <<'USAGE'
Meridian 安装管理工具

用法:
  install.sh install [vX.Y.Z] [-y]   安装最新版或指定版本
  install.sh update [vX.Y.Z] [-y]    更新并自动备份、健康检查、失败回滚
  install.sh status                  显示版本、服务和健康状态
  install.sh restart                 重启服务并执行健康检查
  install.sh logs [行数] [--follow]  查看或持续跟踪 systemd 日志
  install.sh backup                  一致性备份数据库和配置
  install.sh rollback                回滚到上一版本二进制
  install.sh uninstall [-y] [--purge] 卸载；默认保留数据
  install.sh help                    显示帮助

选项:
  -y, --yes       非交互确认
  --purge         卸载时同时删除数据（备份目录不会删除）
  -f, --follow    持续跟踪日志
  -n, --lines N   指定日志行数（1-5000）

不带参数运行时进入交互菜单。
USAGE
}

main_menu() {
    local current choice
    echo ""
    echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
    echo -e "${BOLD}║     Meridian 安装管理工具             ║${NC}"
    echo -e "${BOLD}║     Emby reverse proxy panel         ║${NC}"
    echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"
    echo ""
    current=$(get_current_version)
    echo -e "  当前状态: ${current:-${YELLOW}未安装${NC}}"
    echo ""
    echo "  1) 安装 / 更新"
    echo "  2) 查看状态"
    echo "  3) 重启服务"
    echo "  4) 查看日志"
    echo "  5) 创建备份"
    echo "  6) 回滚版本"
    echo "  7) 卸载"
    echo "  0) 退出"
    echo ""
    read -rp "请选择 [0-7]: " choice
    case "$choice" in
        1) do_install ;;
        2) do_status ;;
        3) do_restart ;;
        4) do_logs ;;
        5) do_backup ;;
        6) do_rollback ;;
        7) do_uninstall ;;
        0) exit 0 ;;
        *) fail "无效选项" ;;
    esac
}

run_cli() {
    local action="${1:-menu}"
    if [ "$#" -gt 0 ]; then
        shift
    fi

    case "$action" in
        remove) action="uninstall" ;;
        -h|--help) action="help" ;;
    esac

    while [ "$#" -gt 0 ]; do
        case "$1" in
            -y|--yes)
                ASSUME_YES=1
                ;;
            --purge)
                PURGE_DATA=1
                ;;
            -f|--follow)
                FOLLOW_LOGS=1
                ;;
            -n|--lines)
                [ "$#" -ge 2 ] || fail "$1 需要一个数字参数"
                LOG_LINES="$2"
                shift
                ;;
            v[0-9]*.*)
                REQUESTED_VERSION="$1"
                ;;
            [0-9]*)
                if [ "$action" = "logs" ]; then
                    LOG_LINES="$1"
                else
                    fail "未知参数: $1"
                fi
                ;;
            -h|--help)
                action="help"
                ;;
            *)
                fail "未知参数: $1"
                ;;
        esac
        shift
    done

    case "$action" in
        install|update) do_install ;;
        status) do_status ;;
        restart) do_restart ;;
        logs) do_logs ;;
        backup) do_backup ;;
        rollback) do_rollback ;;
        uninstall) do_uninstall ;;
        help) usage ;;
        menu) main_menu ;;
        *) fail "未知操作: $action（运行 help 查看帮助）" ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    run_cli "$@"
fi
