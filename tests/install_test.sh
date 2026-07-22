#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TEST_ROOT=$(mktemp -d)

cleanup() {
    if [ "${EUID}" -eq 0 ]; then
        rm -rf -- "$TEST_ROOT"
    else
        sudo rm -rf -- "$TEST_ROOT"
    fi
}
trap cleanup EXIT

export MERIDIAN_INSTALL_DIR="${TEST_ROOT}/bin"
export MERIDIAN_DATA_DIR="${TEST_ROOT}/data"
export MERIDIAN_BACKUP_DIR="${TEST_ROOT}/backups"
export MERIDIAN_SERVICE_FILE="${TEST_ROOT}/meridian.service"
export MERIDIAN_ASSUME_YES=1

# The path is computed so the test also works from an arbitrary checkout path.
# shellcheck disable=SC1091
source "${REPO_ROOT}/install.sh"

assert_eq() {
    local expected="$1" actual="$2" label="$3"
    if [ "$expected" != "$actual" ]; then
        printf 'FAIL: %s: expected %q, got %q\n' "$label" "$expected" "$actual" >&2
        exit 1
    fi
}

assert_file() {
    [ -f "$1" ] || { echo "FAIL: missing file $1" >&2; exit 1; }
}

assert_dir() {
    [ -d "$1" ] || { echo "FAIL: missing directory $1" >&2; exit 1; }
}

valid_version "v1.4.2"
valid_version "v2.0.0-rc.1"
if valid_version "latest"; then
    echo "FAIL: invalid version accepted" >&2
    exit 1
fi

valid_log_lines 1
valid_log_lines 5000
if valid_log_lines 0 || valid_log_lines 5001 || valid_log_lines abc; then
    echo "FAIL: invalid log line count accepted" >&2
    exit 1
fi

for unsafe_path in / /opt/ /opt/../opt /tmp//meridian; do
    if MERIDIAN_DATA_DIR="$unsafe_path" bash -c 'source "$1"; validate_data_dir' _ "${REPO_ROOT}/install.sh" >/dev/null 2>&1; then
        echo "FAIL: unsafe data directory accepted: $unsafe_path" >&2
        exit 1
    fi
done

if MERIDIAN_BACKUP_DIR=/var/ bash -c 'source "$1"; validate_backup_dir' _ "${REPO_ROOT}/install.sh" >/dev/null 2>&1; then
    echo "FAIL: unsafe backup directory accepted" >&2
    exit 1
fi

generate_secret() {
    printf 'test-only-secret-000000000000000000000000000000000000000000000000\n'
}

download() {
    local url="$1" output="$2" version
    version=$(printf '%s' "$url" | awk -F/ '{print $(NF-1)}')
    if [[ "$url" == */SHA256SUMS ]]; then
        printf '%s  meridian-linux-amd64\n' "$(sha256_file "${TEST_ROOT}/release-binary")" > "$output"
        return
    fi
    cat > "${TEST_ROOT}/release-binary" <<BINARY
#!/usr/bin/env sh
if [ "\${1:-}" = "--version" ]; then
    echo "${version}"
fi
BINARY
    chmod 0755 "${TEST_ROOT}/release-binary"
    cp "${TEST_ROOT}/release-binary" "$output"
}

detect_platform() {
    printf 'linux-amd64\n'
}

# Keep the test isolated even when it runs on a host that itself uses systemd.
is_systemd() {
    return 1
}

service_is_active() {
    return 1
}

export REQUESTED_VERSION="v9.9.9"
if ! (do_install) >"${TEST_ROOT}/install-first.log" 2>&1; then
    cat "${TEST_ROOT}/install-first.log" >&2
    exit 1
fi
assert_eq "v9.9.9" "$(get_current_version)" "first installed version"
assert_file "${DATA_DIR}/.env"
assert_eq "9090" "$(read_config_port)" "configured port"

export REQUESTED_VERSION="v9.9.10"
if ! (do_install) >"${TEST_ROOT}/install-update.log" 2>&1; then
    cat "${TEST_ROOT}/install-update.log" >&2
    exit 1
fi
assert_eq "v9.9.10" "$(get_current_version)" "updated version"
assert_eq "v9.9.9" "$(${PREVIOUS_BIN} --version)" "retained previous version"

if ! do_backup >"${TEST_ROOT}/backup.log" 2>&1; then
    cat "${TEST_ROOT}/backup.log" >&2
    exit 1
fi
assert_dir "$BACKUP_DIR"
backup_file=$(as_root find "$BACKUP_DIR" -maxdepth 1 -type f -name '*.tar.gz' -print -quit)
[ -n "$backup_file" ] || { echo "FAIL: backup archive not found" >&2; exit 1; }
as_root test -f "$backup_file" || { echo "FAIL: missing file $backup_file" >&2; exit 1; }
as_root tar -tzf "$backup_file" | grep -q '/.env$'

export PURGE_DATA=0
do_uninstall >"${TEST_ROOT}/uninstall-keep.log" 2>&1
[ ! -e "${INSTALL_DIR}/${BIN_NAME}" ] || { echo "FAIL: binary not removed" >&2; exit 1; }
assert_dir "$DATA_DIR"

export PURGE_DATA=1
do_uninstall >"${TEST_ROOT}/uninstall-purge.log" 2>&1
[ ! -e "$DATA_DIR" ] || { echo "FAIL: data directory not purged" >&2; exit 1; }
assert_dir "$BACKUP_DIR"

help_text=$(usage)
for command_name in install status restart logs backup rollback uninstall; do
    printf '%s' "$help_text" | grep -q "$command_name"
done

echo "installer tests passed"
