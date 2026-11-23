#!/usr/bin/env bash
set -euo pipefail

# Run on a disposable GitHub-hosted Ubuntu runner with real systemd.
# Usage: bash tests/release_install_smoke_test.sh TAG CONTROLLER [PREVIOUS_TAG]
[[ $# -ge 2 && $# -le 3 ]] || { echo 'Expected TAG CONTROLLER [PREVIOUS_TAG]' >&2; exit 2; }
tag=$1
binary=$2
previous_tag=${3:-}
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 2
if [[ -n $previous_tag ]]; then
    [[ $previous_tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ && $previous_tag != "$tag" ]] || exit 2
fi
if [[ ${RUNNER_ENVIRONMENT:-} != github-hosted || ! -d /run/systemd/system ]]; then
    echo 'The native release test requires a disposable GitHub-hosted systemd runner' >&2
    exit 1
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
release_repo=chanhui800/Meridian
case $(uname -m) in
    x86_64) asset=meridian-linux-amd64 ;;
    aarch64) asset=meridian-linux-arm64 ;;
    *) echo 'Unsupported runner architecture' >&2; exit 1 ;;
esac

work_dir=$(sudo -n mktemp -d /opt/meridian-release-smoke.XXXXXXXX)
service_account=meridian-smoke-${work_dir##*.}
unit=$service_account.service
unit_file=/etc/systemd/system/$unit
drop_in=$unit_file.d/loopback.conf
scratch=$work_dir/private
phase=setup
owns_unit=0

cleanup() {
    local status=$?
    trap - EXIT
    if (( status != 0 )); then
        printf 'Native release test failed during %s\n' "$phase" >&2
        sudo -n systemctl show "$unit" --property=ActiveState,SubState,Result,ExecMainStatus >&2 || true
        if [[ -f $scratch/installer.log ]]; then
            awk '/初始化令牌/ { next } { print }' "$scratch/installer.log" >&2
        fi
    fi
    if (( owns_unit )); then
        if [[ -f $unit_file ]]; then
            sudo -n systemctl disable --now "$unit" >/dev/null 2>&1 || status=1
        fi
        sudo -n rm -f -- "$unit_file" "$drop_in" || status=1
        sudo -n rmdir -- "$unit_file.d" || status=1
        sudo -n systemctl daemon-reload || status=1
        sudo -n systemctl reset-failed "$unit" >/dev/null 2>&1 || true
        if getent passwd "$service_account" >/dev/null; then
            sudo -n userdel "$service_account" || status=1
        fi
        if getent group "$service_account" >/dev/null; then
            sudo -n groupdel "$service_account" || status=1
        fi
    fi
    case $work_dir in
        /opt/meridian-release-smoke.*) sudo -n rm -rf -- "$work_dir" || status=1 ;;
        *) status=1 ;;
    esac
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -e $unit_file || -L $unit_file || -e $unit_file.d || -L $unit_file.d ]] \
    || getent passwd "$service_account" >/dev/null || getent group "$service_account" >/dev/null; then
    echo 'The temporary service name is already in use' >&2
    exit 1
fi
sudo -n mkdir -- "$unit_file.d"
owns_unit=1
sudo -n chown "$(id -u):$(id -g)" "$work_dir"
chmod 0711 "$work_dir"
mkdir -m 0700 -- "$scratch"
mkdir -m 0711 -- "$work_dir/fresh" "$work_dir/upgrade"
cp -- "$binary" "$scratch/$asset"
chmod 0755 "$scratch/$asset"
[[ $("$scratch/$asset" --version) == "$tag" ]]
[[ $("$scratch/$asset" --build-mode) == controller ]]
(cd "$scratch" && sha256sum "$asset" > SHA256SUMS)

port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
panel=http://127.0.0.1:$port
# A second EnvironmentFile narrows the actual listener before the first start.
# /opt also keeps the installation visible inside the service's PrivateTmp.
printf 'PORT=%s\nPANEL_BIND_ADDR=127.0.0.1\n' "$port" > "$work_dir/listener.env"
printf '[Service]\nEnvironmentFile=%s/listener.env\n' "$work_dir" | sudo -n tee "$drop_in" >/dev/null

run_installer() (
    local installer=$1 install_root=$2 requested_tag=$3
    shift 3
    export MERIDIAN_REPO="$release_repo" MERIDIAN_INSTALL_DIR="$install_root/bin"
    export MERIDIAN_DATA_DIR="$install_root/data" MERIDIAN_BACKUP_DIR="$install_root/backups"
    export MERIDIAN_SERVICE_FILE="$unit_file" MERIDIAN_SERVICE_NAME="$unit"
    export MERIDIAN_NGINX_ROOT="$install_root/nginx"
    export MERIDIAN_NGINX_CONFIG="$install_root/nginx/conf.d/meridian-panel.conf"
    export MERIDIAN_ROOT_GROUP=root MERIDIAN_BUNDLE_AGENT_BINARIES=0
    # shellcheck disable=SC1090
    source "$installer"
    # These variables and functions are consumed by the sourced installer.
    # shellcheck disable=SC2034
    SERVICE_USER=$service_account
    # shellcheck disable=SC2034
    SERVICE_GROUP=$service_account
    # shellcheck disable=SC2329,SC2317
    get_latest_version() { printf '%s\n' "$requested_tag"; }
    if [[ $requested_tag == "$tag" ]]; then
        # The candidate is not published yet. Supply only its release bytes.
        # The actual installer still verifies SHA-256, installs, and starts it.
        # shellcheck disable=SC2329,SC2317
        download() {
            case $1 in
                "https://github.com/$release_repo/releases/download/$tag/$asset") cp -- "$scratch/$asset" "$2" ;;
                "https://github.com/$release_repo/releases/download/$tag/SHA256SUMS") cp -- "$scratch/SHA256SUMS" "$2" ;;
                *) printf 'Unexpected candidate download request\n' >&2; return 1 ;;
            esac
        }
    fi
    run_cli "$@"
)

local_http() { curl --noproxy '*' --connect-timeout 2 --max-time 10 -fsS "$@"; }

verify_running() {
    local install_root=$1 expected_tag=$2
    [[ $("$install_root/bin/meridian" --version) == "$expected_tag" ]]
    sudo -n systemctl is-active --quiet "$unit"
    sudo -n env DB_PATH="$install_root/data/meridian.db" "$install_root/bin/meridian" --healthcheck
    local_http "$panel/api/auth/check" | jq -e '.mode == "single_admin"' >/dev/null
}

verify_embedded_files() {
    local_http "$panel/" > "$scratch/index.html"
    grep -Fq '/js/app.js' "$scratch/index.html"
    local_http "$panel/js/app.js" > "$scratch/app.js"
    cmp -s -- "$scratch/app.js" "$repo_root/web/static/js/app.js"
    local_http "$panel/api/agent/install.sh" > "$scratch/agent-install.sh"
    cmp -s -- "$scratch/agent-install.sh" "$repo_root/scripts/agent-install.sh"
}

schema_version() {
    sudo -n python3 - "$1/data/meridian.db" <<'PY'
import sqlite3, sys
with sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True) as db:
    print(db.execute("PRAGMA user_version").fetchone()[0])
PY
}

phase='fresh installation'
run_installer "$repo_root/install.sh" "$work_dir/fresh" "$tag" install --no-domain -y > "$scratch/installer.log" 2>&1
verify_running "$work_dir/fresh" "$tag"
local_http "$panel/api/auth/check" | jq -e '.needs_setup' >/dev/null
verify_embedded_files
fresh_schema=$(schema_version "$work_dir/fresh")
printf 'Fresh %s installation serves the panel and embedded Agent installer (schema %s)\n' "$tag" "$fresh_schema"

if [[ -n $previous_tag ]]; then
    phase='predecessor installation'
    run_installer "$repo_root/install.sh" "$work_dir/fresh" "$tag" uninstall -y > "$scratch/installer.log" 2>&1
    prior_base="https://github.com/$release_repo/releases/download/$previous_tag"
    curl --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 60 --retry 3 -fsSL \
        "$prior_base/install.sh" -o "$scratch/previous-install.sh"
    curl --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 60 --retry 3 -fsSL \
        "$prior_base/SHA256SUMS" -o "$scratch/previous-sums"
    prior_sum=$(awk '$2 == "install.sh" || $2 == "*install.sh" {print $1; exit}' "$scratch/previous-sums")
    [[ $prior_sum =~ ^[[:xdigit:]]{64}$ ]]
    printf '%s  %s\n' "$prior_sum" "$scratch/previous-install.sh" | sha256sum -c --status

    # The predecessor installer uses the configured port in its own probe.
    # Seed only the port and required identity fields for this empty database.
    mkdir -m 0700 -- "$work_dir/upgrade/data"
    (
        umask 077
        printf 'PORT=%s\nDB_PATH=%s/upgrade/data/meridian.db\nJWT_SECRET=%s\n' \
            "$port" "$work_dir" "$(openssl rand -hex 32)" > "$work_dir/upgrade/data/.env"
    )
    run_installer "$scratch/previous-install.sh" "$work_dir/upgrade" "$previous_tag" install --no-domain -y > "$scratch/installer.log" 2>&1
    verify_running "$work_dir/upgrade" "$previous_tag"
    prior_schema=$(schema_version "$work_dir/upgrade")
    password=$(openssl rand -hex 20)
    # Keep the one-time setup token and password out of process arguments and logs.
    {
        sudo -n sed -n 's/^SETUP_TOKEN=//p' "$work_dir/upgrade/data/.env"
        printf '%s\n' "$password"
    } | python3 -c 'import json,sys; token,password = sys.stdin.read().splitlines(); print(json.dumps(dict(username="release-smoke",password=password,setup_token=token)))' \
        | local_http -H "Origin: $panel" -H 'Content-Type: application/json' --data-binary @- "$panel/api/auth/setup" \
        | jq -e '.username == "release-smoke"' >/dev/null

    phase='upgrade and account preservation'
    run_installer "$repo_root/install.sh" "$work_dir/upgrade" "$tag" update -y > "$scratch/installer.log" 2>&1
    verify_running "$work_dir/upgrade" "$tag"
    verify_embedded_files
    upgraded_schema=$(schema_version "$work_dir/upgrade")
    [[ $upgraded_schema == "$fresh_schema" && $upgraded_schema -ge $prior_schema ]]
    printf '%s\n' "$password" | python3 -c 'import json,sys; print(json.dumps(dict(username="release-smoke",password=sys.stdin.readline().rstrip("\n"))))' \
        | local_http -H "Origin: $panel" -H 'Content-Type: application/json' --data-binary @- \
            --cookie-jar "$scratch/session" "$panel/api/auth/login" >/dev/null
    local_http --cookie "$scratch/session" "$panel/api/auth/check" \
        | jq -e '.authenticated and .username == "release-smoke" and (.needs_setup | not)' >/dev/null
    printf 'Upgraded %s to %s, preserved login, and verified SQLite schema %s → %s\n' \
        "$previous_tag" "$tag" "$prior_schema" "$upgraded_schema"
fi
