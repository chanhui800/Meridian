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
binary_tmp="$install_dir/meridian-agent.tmp"
headers_tmp="$install_dir/meridian-agent.headers.tmp"

[ "$(id -u)" -eq 0 ] || { echo 'Please run this script as root.' >&2; exit 1; }
case "$(uname -s):$(uname -m)" in
  Linux:x86_64|Linux:amd64|Linux:aarch64|Linux:arm64) ;;
  *) echo 'This Agent installer supports Linux amd64 and arm64 only.' >&2; exit 1 ;;
esac

command -v curl >/dev/null 2>&1 || { echo 'curl is required.' >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo 'systemd is required.' >&2; exit 1; }
install -d -m 0755 "$install_dir"
install -d -m 0700 "$state_dir" "$token_dir"
systemctl stop meridian-agent.service 2>/dev/null || true
rm -f "$state_dir/state.json" "$binary_tmp" "$headers_tmp"
umask 077
printf '%s' "$enrollment_token" > "$token_file"
curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -D "$headers_tmp" \
  -H "Authorization: Bearer $enrollment_token" \
  "$controller_url/api/agent/binary" -o "$binary_tmp"
expected_sha=$(awk 'BEGIN{IGNORECASE=1} /^X-Meridian-Agent-SHA256:/ {gsub(/[[:space:]]/, "", $2); print $2; exit}' "$headers_tmp" | tr -d '\r')
if command -v sha256sum >/dev/null 2>&1; then
  actual_sha=$(sha256sum "$binary_tmp" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual_sha=$(shasum -a 256 "$binary_tmp" | awk '{print $1}')
else
  echo 'sha256sum or shasum is required.' >&2
  exit 1
fi
if [ -z "$expected_sha" ] || [ "$(printf '%s' "$expected_sha" | tr '[:upper:]' '[:lower:]')" != "$(printf '%s' "$actual_sha" | tr '[:upper:]' '[:lower:]')" ]; then
  echo 'Agent binary checksum mismatch.' >&2
  exit 1
fi
rm -f "$headers_tmp"
chmod 0755 "$binary_tmp"
mv -f "$binary_tmp" "$install_dir/meridian-agent"

cat > /etc/systemd/system/meridian-agent.service <<EOF
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

systemctl daemon-reload
systemctl enable --now meridian-agent.service
if ! systemctl is-active --quiet meridian-agent.service; then
  systemctl --no-pager --full --lines=20 status meridian-agent.service >&2 || true
  exit 1
fi
printf '%s\n' 'Meridian Agent installed and running.'
