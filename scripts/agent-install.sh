#!/bin/sh
set -eu

# Meridian Agent installer. The script is public; the enrollment token is
# supplied by the operator and is never stored in this repository.
controller_url=""
enrollment_token=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|-e|--controller|--endpoint)
      [ "$#" -ge 2 ] || { echo 'missing controller URL' >&2; exit 2; }
      controller_url="$2"
      shift 2
      ;;
    -t|--token)
      [ "$#" -ge 2 ] || { echo 'missing enrollment token' >&2; exit 2; }
      enrollment_token="$2"
      shift 2
      ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$controller_url" ] || [ -z "$enrollment_token" ]; then
  echo 'usage: agent-install.sh -e https://panel.example.com:9090 -t ENROLLMENT_TOKEN' >&2
  exit 2
fi
controller_url=${controller_url%/}
install_dir=/opt/meridian-agent
state_dir=/var/lib/meridian-agent
token_dir=/etc/meridian-agent
token_file="$token_dir/enrollment-token"
service_file=/etc/systemd/system/meridian-agent.service
umask 077
rollback_active=0

[ "$(id -u)" -eq 0 ] || { echo 'Please run this script as root.' >&2; exit 1; }
case "$(uname -s):$(uname -m)" in
  Linux:x86_64|Linux:amd64) agent_platform='linux/amd64' ;;
  Linux:aarch64|Linux:arm64) agent_platform='linux/arm64' ;;
  *) echo 'This Agent installer supports Linux amd64 and arm64 only.' >&2; exit 1 ;;
esac

command -v curl >/dev/null 2>&1 || { echo 'curl is required.' >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo 'systemd is required.' >&2; exit 1; }
command -v mktemp >/dev/null 2>&1 || { echo 'mktemp is required.' >&2; exit 1; }
install -d -m 0755 "$install_dir"
install -d -m 0700 "$state_dir" "$token_dir"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/meridian-agent-install.XXXXXXXX")
cleanup() {
  exit_code=$?
  if [ "$exit_code" -ne 0 ] && [ "$rollback_active" -eq 1 ] && command -v rollback >/dev/null 2>&1; then
    rollback || true
  fi
  rm -rf "$work_dir"
  exit "$exit_code"
}
trap cleanup EXIT
binary_tmp="$work_dir/meridian-agent"
headers_tmp="$work_dir/meridian-agent.headers"
token_tmp="$work_dir/enrollment-token"
service_tmp="$work_dir/meridian-agent.service"

# Download and validate everything before touching the running service, state,
# token, or systemd unit. A failed download therefore leaves an existing Agent
# completely untouched.
curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -D "$headers_tmp" \
  -H "Authorization: Bearer $enrollment_token" \
  -H "X-Meridian-Agent-Platform: $agent_platform" \
  "$controller_url/api/agent/binary" -o "$binary_tmp"
served_platform=$(awk 'tolower($1) == "x-meridian-agent-platform:" {gsub(/\r/, "", $2); print tolower($2); exit}' "$headers_tmp")
if [ "$served_platform" != "$agent_platform" ]; then
  echo "Agent binary platform mismatch (requested $agent_platform, received ${served_platform:-unknown})." >&2
  exit 1
fi
expected_sha=$(awk 'tolower($1) == "x-meridian-agent-sha256:" {gsub(/\r/, "", $2); print tolower($2); exit}' "$headers_tmp")
if command -v sha256sum >/dev/null 2>&1; then
  actual_sha=$(sha256sum "$binary_tmp" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual_sha=$(shasum -a 256 "$binary_tmp" | awk '{print $1}')
else
  echo 'sha256sum or shasum is required.' >&2
  exit 1
fi
actual_sha=$(printf '%s' "$actual_sha" | tr '[:upper:]' '[:lower:]')
if [ -z "$expected_sha" ]; then
  echo 'Agent download response is missing X-Meridian-Agent-SHA256.' >&2
  exit 1
fi
if [ "$expected_sha" != "$actual_sha" ]; then
  echo 'Agent binary checksum mismatch.' >&2
  echo "expected: $expected_sha" >&2
  echo "actual:   $actual_sha" >&2
  exit 1
fi
chmod 0755 "$binary_tmp"
printf '%s' "$enrollment_token" > "$token_tmp"
chmod 0600 "$token_tmp"
cat > "$service_tmp" <<EOF
[Unit]
Description=Meridian node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$install_dir/meridian-agent --controller $controller_url --state $state_dir/state.json --enroll-token-file $token_file
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=$state_dir $install_dir $token_dir

[Install]
WantedBy=multi-user.target
EOF

# Snapshot the small set of files that will be replaced. state.json is
# deliberately never removed: reinstalling the binary must not force a new
# enrollment or destroy the node's durable state.
had_binary=0; had_token=0; had_service=0; was_active=0; was_enabled=0
binary_backup="$work_dir/meridian-agent.previous"
token_backup="$work_dir/enrollment-token.previous"
service_backup="$work_dir/meridian-agent.service.previous"
if [ -f "$install_dir/meridian-agent" ]; then
  had_binary=1
  cp -p "$install_dir/meridian-agent" "$binary_backup"
fi
if [ -f "$token_file" ]; then
  had_token=1
  cp -p "$token_file" "$token_backup"
fi
if [ -f "$service_file" ]; then
  had_service=1
  cp -p "$service_file" "$service_backup"
fi
if systemctl is-active --quiet meridian-agent.service 2>/dev/null; then was_active=1; fi
if systemctl is-enabled --quiet meridian-agent.service 2>/dev/null; then was_enabled=1; fi

rollback() {
  systemctl stop meridian-agent.service >/dev/null 2>&1 || true
  if [ "$had_binary" -eq 1 ]; then
    install -m 0755 "$binary_backup" "$install_dir/meridian-agent.rollback"
    mv -f "$install_dir/meridian-agent.rollback" "$install_dir/meridian-agent"
  else
    rm -f "$install_dir/meridian-agent"
  fi
  if [ "$had_token" -eq 1 ]; then
    install -m 0600 "$token_backup" "$token_file.rollback"
    mv -f "$token_file.rollback" "$token_file"
  else
    rm -f "$token_file"
  fi
  if [ "$had_service" -eq 1 ]; then
    install -m 0644 "$service_backup" "$service_file.rollback"
    mv -f "$service_file.rollback" "$service_file"
  else
    rm -f "$service_file"
  fi
  systemctl daemon-reload >/dev/null 2>&1 || true
  if [ "$was_enabled" -eq 1 ]; then systemctl enable meridian-agent.service >/dev/null 2>&1 || true; else systemctl disable meridian-agent.service >/dev/null 2>&1 || true; fi
  if [ "$was_active" -eq 1 ]; then systemctl start meridian-agent.service >/dev/null 2>&1 || true; fi
}
trap 'if [ "$rollback_active" -eq 1 ]; then rollback; fi; exit 130' HUP INT TERM

systemctl stop meridian-agent.service >/dev/null 2>&1 || true
rollback_active=1
install -m 0755 "$binary_tmp" "$install_dir/meridian-agent.new"
mv -f "$install_dir/meridian-agent.new" "$install_dir/meridian-agent"
# Preserve a valid existing enrollment token and state. A new token is only
# written for a first install or when the state file is absent (explicit
# re-enrollment can remove state.json before running this script).
if [ ! -f "$state_dir/state.json" ] || [ ! -f "$token_file" ]; then
  install -m 0600 "$token_tmp" "$token_file.new"
  mv -f "$token_file.new" "$token_file"
fi
install -m 0644 "$service_tmp" "$service_file.new"
mv -f "$service_file.new" "$service_file"
if ! systemctl daemon-reload || ! systemctl enable --now meridian-agent.service || ! systemctl is-active --quiet meridian-agent.service; then
  rollback
  rollback_active=0
  echo 'Agent installation failed; the previous Agent, state, token, and service were restored.' >&2
  exit 1
fi
rollback_active=0
printf '%s\n' 'Meridian Agent installed and running.'
