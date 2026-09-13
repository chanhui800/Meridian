#!/bin/sh
set -eu

# Meridian Agent installer. The script is public; the enrollment token is
# supplied by the operator and is never stored in this repository.
controller_url=""
enrollment_token=""
reenroll=0
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
    --reenroll)
      reenroll=1
      shift
      ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$controller_url" ] || [ -z "$enrollment_token" ]; then
  echo 'usage: agent-install.sh -e https://panel.example.com:9090 -t ENROLLMENT_TOKEN [--reenroll]' >&2
  exit 2
fi
controller_url=${controller_url%/}
install_dir=/opt/meridian-agent
state_dir=/var/lib/meridian-agent
token_dir=/etc/meridian-agent
token_file="$token_dir/enrollment-token"
state_file="$state_dir/state.json"
service_file=/etc/systemd/system/meridian-agent.service
umask 077
rollback_active=0
enrollment_committed=0

[ "$(id -u)" -eq 0 ] || { echo 'Please run this script as root.' >&2; exit 1; }
case "$(uname -s):$(uname -m)" in
  Linux:x86_64|Linux:amd64) agent_platform='linux/amd64' ;;
  Linux:aarch64|Linux:arm64) agent_platform='linux/arm64' ;;
  *) echo 'This Agent installer supports Linux amd64 and arm64 only.' >&2; exit 1 ;;
esac

command -v curl >/dev/null 2>&1 || { echo 'curl is required.' >&2; exit 1; }
command -v mktemp >/dev/null 2>&1 || { echo 'mktemp is required.' >&2; exit 1; }

# The Agent is supervised by the system init: it exits with a non-zero status
# after replacing its own binary (self-update) and relies entirely on the init
# system to start it again, so the unit written below must respawn. Both
# supported init systems are detected explicitly, and an unsupported one is
# reported by name instead of failing later with a misleading message.
init_system=''
if command -v systemctl >/dev/null 2>&1; then
  init_system='systemd'
elif command -v openrc >/dev/null 2>&1 || command -v rc-service >/dev/null 2>&1; then
  init_system='openrc'
else
  echo 'This Agent installer supports systemd and OpenRC only.' >&2
  echo "Detected init: $(cat /proc/1/comm 2>/dev/null || echo unknown)" >&2
  echo 'Alpine and other OpenRC systems are supported; a container without an init system is not.' >&2
  exit 1
fi
service_file=
openrc_service_file=/etc/init.d/meridian-agent
case "$init_system" in
  systemd) service_file=/etc/systemd/system/meridian-agent.service ;;
esac

# --- service lifecycle -------------------------------------------------------
# One indirection layer so the install/rollback logic below is identical for
# both init systems. Every helper tolerates "already stopped" / "already
# running" states, because the installer re-runs over an existing install.
cmd_service_stop() {
  case "$init_system" in
    systemd) systemctl stop meridian-agent.service >/dev/null 2>&1 || true ;;
    openrc) rc-service meridian-agent stop >/dev/null 2>&1 || true ;;
  esac
}
cmd_service_enable() {
  if [ "$1" -eq 1 ]; then
    case "$init_system" in
      systemd) systemctl enable meridian-agent.service >/dev/null 2>&1 || true ;;
      openrc) rc-update add meridian-agent default >/dev/null 2>&1 || true ;;
    esac
  else
    case "$init_system" in
      systemd) systemctl disable meridian-agent.service >/dev/null 2>&1 || true ;;
      openrc) rc-update del meridian-agent default >/dev/null 2>&1 || true ;;
    esac
  fi
}
cmd_service_start() {
  case "$init_system" in
    systemd) systemctl start meridian-agent.service >/dev/null 2>&1 ;;
    openrc) rc-service meridian-agent start >/dev/null 2>&1 ;;
  esac
}
cmd_service_active() {
  case "$init_system" in
    systemd) systemctl is-active --quiet meridian-agent.service 2>/dev/null ;;
    openrc) rc-service meridian-agent status >/dev/null 2>&1 ;;
  esac
}
cmd_service_enabled() {
  case "$init_system" in
    systemd) systemctl is-enabled --quiet meridian-agent.service 2>/dev/null ;;
    openrc) rc-update show default 2>/dev/null | grep -Eq '(^|[[:space:]])meridian-agent([[:space:]]|$)' ;;
  esac
}
cmd_service_reload() {
  case "$init_system" in
    systemd) systemctl daemon-reload >/dev/null 2>&1 || true ;;
    openrc) : ;;
  esac
}
cmd_service_install() {
  # $1 = path to the freshly rendered unit/init script
  case "$init_system" in
    systemd)
      install -m 0644 "$1" "$service_file.new"
      mv -f "$service_file.new" "$service_file"
      ;;
    openrc)
      install -m 0755 "$1" "$openrc_service_file.new"
      mv -f "$openrc_service_file.new" "$openrc_service_file"
      ;;
  esac
}
write_agent_service_unit() {
  # $1 = destination path
  case "$init_system" in
    systemd)
      cat > "$1" <<EOF
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
PrivateDevices=true
ProtectHome=true
ProtectSystem=strict
ProtectHostname=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictRealtime=true
ReadWritePaths=$state_dir $install_dir $token_dir

[Install]
WantedBy=multi-user.target
EOF
      ;;
    openrc)
      # supervise-daemon plus respawn is the OpenRC equivalent of systemd's
      # Restart=always: the Agent exits(1) after a self-update, so without a
      # supervised respawn a node would stay offline after its first update.
      cat > "$1" <<EOF
#!/sbin/openrc-run
# Managed by the Meridian Agent installer.
name="meridian-agent"
description="Meridian node agent"
command="$install_dir/meridian-agent"
command_args="--controller $controller_url --state $state_dir/state.json --enroll-token-file $token_file"
command_user="root"
command_background="yes"
pidfile="/run/\${RC_SVCNAME:-meridian-agent}.pid"
supervisor="supervise-daemon"
respawn_delay=5
respawn_max=0
output_log="/var/log/meridian-agent.log"
error_log="/var/log/meridian-agent.log"
rc_ulimit="-n 4096"
depend() {
  need net
  after firewall
}
EOF
      ;;
  esac
}

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
manifest_headers_tmp="$work_dir/meridian-agent.manifest.headers"
token_tmp="$work_dir/enrollment-token"
service_tmp="$work_dir/meridian-agent.service"

# Download the release manifest from the controller, then fetch the immutable
# GitHub asset directly. This keeps the Controller image free of per-platform
# Agent binaries while retaining a controller-authenticated enrollment step.
curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -D "$manifest_headers_tmp" \
  -H "Authorization: Bearer $enrollment_token" \
  -H "X-Meridian-Agent-Platform: $agent_platform" \
  "$controller_url/api/agent/manifest" -o "$work_dir/agent-manifest.json"
served_platform=$(awk 'tolower($1) == "x-meridian-agent-platform:" {gsub(/\r/, "", $2); print tolower($2); exit}' "$manifest_headers_tmp")
if [ "$served_platform" != "$agent_platform" ]; then
  echo "Agent binary platform mismatch (requested $agent_platform, received ${served_platform:-unknown})." >&2
  exit 1
fi
download_url=$(awk 'tolower($1) == "x-meridian-agent-download-url:" {sub(/^[^:]*:[[:space:]]*/, ""); gsub(/\r/, ""); print; exit}' "$manifest_headers_tmp")
case "$download_url" in
  https://github.com/chanhui800/Meridian/releases/download/*/meridian-agent-linux-amd64|https://github.com/chanhui800/Meridian/releases/download/*/meridian-agent-linux-arm64) ;;
  *) echo 'Agent release manifest returned an invalid download URL.' >&2; exit 1 ;;
esac
expected_sha=$(awk 'tolower($1) == "x-meridian-agent-sha256:" {gsub(/\r/, "", $2); print tolower($2); exit}' "$manifest_headers_tmp")
if ! printf '%s' "$expected_sha" | grep -Eq '^[[:xdigit:]]{64}$'; then
  echo 'Agent release manifest returned an invalid SHA-256.' >&2
  exit 1
fi
curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -D "$headers_tmp" \
  "$download_url" -o "$binary_tmp"
served_platform=$(awk 'tolower($1) == "x-meridian-agent-platform:" {gsub(/\r/, "", $2); print tolower($2); exit}' "$headers_tmp")
if [ -n "$served_platform" ] && [ "$served_platform" != "$agent_platform" ]; then
  echo "Agent binary platform mismatch (requested $agent_platform, received $served_platform)." >&2
  exit 1
fi
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
write_agent_service_unit "$service_tmp"

# Snapshot the small set of files that will be replaced. A normal reinstall
# keeps state.json; an explicit re-enrollment snapshots it so rollback can
# restore the local files if the new enrollment fails.
had_binary=0; had_token=0; had_state=0; had_service=0; was_active=0; was_enabled=0
binary_backup="$work_dir/meridian-agent.previous"
token_backup="$work_dir/enrollment-token.previous"
state_backup="$work_dir/state.json.previous"
service_backup="$work_dir/meridian-agent.service.previous"
if [ -f "$install_dir/meridian-agent" ]; then
  had_binary=1
  cp -p "$install_dir/meridian-agent" "$binary_backup"
fi
if [ -f "$token_file" ]; then
  had_token=1
  cp -p "$token_file" "$token_backup"
fi
if [ -f "$state_file" ]; then
  had_state=1
  cp -p "$state_file" "$state_backup"
fi
previous_state_token=""
if [ "$had_state" -eq 1 ]; then
  previous_state_token=$(sed -n 's/.*"agent_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$state_file" | head -n 1)
fi
managed_service_file="$service_file"
if [ "$init_system" = 'openrc' ]; then
  managed_service_file="$openrc_service_file"
fi
if [ -f "$managed_service_file" ]; then
  had_service=1
  cp -p "$managed_service_file" "$service_backup"
fi
if cmd_service_active; then was_active=1; fi
if cmd_service_enabled; then was_enabled=1; fi

rollback() {
  cmd_service_stop
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
  if [ "$enrollment_committed" -eq 1 ]; then
    # The Controller has already atomically replaced the old Agent token.
    # Keep the newly enrolled state so rollback never restores credentials
    # that the server has deliberately invalidated.
    rm -f "$token_file"
  elif [ "$had_state" -eq 1 ]; then
    install -m 0600 "$state_backup" "$state_file.rollback"
    mv -f "$state_file.rollback" "$state_file"
  else
    rm -f "$state_file"
  fi
  if [ "$had_service" -eq 1 ]; then
    install -m 0755 "$service_backup" "$managed_service_file.rollback"
    mv -f "$managed_service_file.rollback" "$managed_service_file"
  else
    rm -f "$managed_service_file"
  fi
  cmd_service_reload
  if [ "$was_enabled" -eq 1 ]; then cmd_service_enable 1; else cmd_service_enable 0; fi
  if [ "$was_active" -eq 1 ]; then cmd_service_start || true; fi
}
trap 'if [ "$rollback_active" -eq 1 ]; then rollback; rollback_active=0; fi; exit 130' HUP INT TERM

cmd_service_stop
rollback_active=1
install -m 0755 "$binary_tmp" "$install_dir/meridian-agent.new"
mv -f "$install_dir/meridian-agent.new" "$install_dir/meridian-agent"
# A normal reinstall keeps durable enrollment state. The explicit re-enroll
# path is used by the panel's "regenerate script" action and forces the Agent
# to exchange the fresh one-time enrollment token.
if [ "$reenroll" -eq 1 ]; then
  rm -f "$state_file"
  install -m 0600 "$token_tmp" "$token_file.new"
  mv -f "$token_file.new" "$token_file"
elif [ ! -f "$state_file" ]; then
  install -m 0600 "$token_tmp" "$token_file.new"
  mv -f "$token_file.new" "$token_file"
fi
cmd_service_install "$service_tmp"
wait_for_registration() {
  # An init system may report the service as running while the Agent is still
  # retrying an invalid token. Enrollment writes a new state file and removes
  # the one-time token; wait for both signals before reporting success for a
  # first install/re-enroll.
  if [ "$reenroll" -ne 1 ] && [ "$had_state" -ne 0 ]; then
    return 0
  fi
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    if [ -s "$state_file" ] \
      && grep -Eq '"node_guid"[[:space:]]*:[[:space:]]*"[^"]+"' "$state_file" \
      && grep -Eq '"agent_token"[[:space:]]*:[[:space:]]*"[^"]+"' "$state_file"; then
      current_state_token=$(sed -n 's/.*"agent_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$state_file" | head -n 1)
      if [ "$had_state" -eq 0 ] || [ "$current_state_token" != "$previous_state_token" ]; then
        enrollment_committed=1
      fi
      if cmd_service_active && [ ! -e "$token_file" ]; then
        return 0
      fi
    fi
    attempt=$((attempt + 1))
    sleep 2
  done
  return 1
}
# Enable first, then start, so a failure to register at boot is still reported
# the same way on both init systems.
cmd_service_reload
cmd_service_enable 1
if ! cmd_service_start || ! cmd_service_active || ! wait_for_registration; then
  rollback
  rollback_active=0
  if [ "$enrollment_committed" -eq 1 ]; then
    echo 'Agent installation did not finish, but the new Controller enrollment was committed; the new state was kept for recovery.' >&2
  else
    echo 'Agent installation failed; the previous Agent, state, token, and service were restored.' >&2
  fi
  exit 1
fi
rollback_active=0
printf '%s\n' 'Meridian Agent installed and running.'
