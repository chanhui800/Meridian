#!/usr/bin/env bash
# Behavioural tests for scripts/agent-install.sh init-system support.
#
# Runs the REAL installer inside a real Alpine (musl) container with PATH stubs
# only for the network and the service manager, then asserts what it created.
# Alpine is the reason this exists: it uses OpenRC instead of systemd, and
# busybox ash is /bin/sh there.
#
# Scenarios:
#   openrc  - rc-service present, no systemctl  -> /etc/init.d script
#   systemd - systemctl present                 -> systemd unit
#   none    - neither                           -> refuses, naming both
#
# Requires docker. Skips (exit 0) when docker or the alpine image is unavailable,
# so it is safe to run anywhere; CI runs it where a daemon exists.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="${REPO_ROOT}/scripts/agent-install.sh"
IMAGE="${MERIDIAN_TEST_ALPINE_IMAGE:-alpine:3.20}"

if ! command -v docker >/dev/null 2>&1; then
    echo 'SKIP: docker is not available'
    exit 0
fi
if ! docker info >/dev/null 2>&1; then
    echo 'SKIP: no docker daemon reachable'
    exit 0
fi
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    if ! docker pull --platform linux/amd64 "$IMAGE" >/dev/null 2>&1; then
        echo "SKIP: cannot obtain $IMAGE"
        exit 0
    fi
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# The repository stores LF, but a Windows checkout with core.autocrlf=true hands
# us CRLF, and a bare CR makes busybox ash reject "set -e\r" as an illegal
# option. Normalising here keeps that harness artifact out of the result.
tr -d '\r' < "$SCRIPT" > "$WORK/agent-install.sh"

cat > "$WORK/harness.sh" <<'CONTAINER'
set -eu
SCENARIO="${1:?}"
SB=/tmp/sb
# The PATH stubs are separate processes and read $SB too, so it must be exported.
export SB
rm -rf "$SB"
mkdir -p "$SB/bin-common" "$SB/bin-init" "$SB/tmp"

cat > "$SB/bin-common/id" <<'EOF'
#!/bin/sh
[ "${1:-}" = "-u" ] && { echo 0; exit 0; }
echo root
EOF
# aarch64 models the reported host; the unit body is architecture independent.
cat > "$SB/bin-common/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in -s) echo Linux ;; -m) echo aarch64 ;; *) echo Linux ;; esac
EOF
cat > "$SB/bin-common/mktemp" <<'EOF'
#!/bin/sh
dir="${2:-${1:-/tmp/x}}"
mkdir -p "$dir"
echo "$dir"
EOF
cat > "$SB/bin-common/curl" <<'EOF'
#!/bin/sh
out=""; hdr=""; prev=""
for a in "$@"; do
  case "$prev" in -o) out="$a" ;; -D) hdr="$a" ;; esac
  prev="$a"
done
url=""
for a in "$@"; do case "$a" in https://*) url="$a" ;; esac; done
case "$url" in
  */api/agent/manifest)
    if [ -n "$hdr" ]; then
      { echo "HTTP/1.1 200 OK"
        echo "X-Meridian-Agent-Platform: linux/arm64"
        echo "X-Meridian-Agent-Version: v9.9.9"
        echo "X-Meridian-Agent-SHA256: $(cat "$SB/expected-sha")"
        echo "X-Meridian-Agent-Download-URL: https://github.com/chanhui800/Meridian/releases/download/v9.9.9/meridian-agent-linux-arm64"
      } > "$hdr"
    fi
    [ -n "$out" ] && printf '{}' > "$out"
    ;;
  */releases/download/*)
    # Real curl always writes the -D header file; the installer reads it to
    # confirm the platform, so the stub must create it too.
    if [ -n "$hdr" ]; then
      { echo "HTTP/1.1 200 OK"
        echo "X-Meridian-Agent-Platform: linux/arm64"
      } > "$hdr"
    fi
    cat > "$out" <<'AGENT'
#!/bin/sh
exit 0
AGENT
    chmod +x "$out"
    ;;
esac
exit 0
EOF
cat > "$SB/bin-common/sha256sum" <<'EOF'
#!/bin/sh
echo "$(cat "$SB/expected-sha")  $1"
EOF
chmod +x "$SB/bin-common/"*

case "$SCENARIO" in
openrc)
  cat > "$SB/bin-init/openrc" <<'EOF'
#!/bin/sh
echo "OpenRC 0.54"
EOF
  # A stub cannot really launch the agent, so "started" is tracked in a marker
  # file: the installer verifies the service is active after starting it.
  cat > "$SB/bin-init/rc-service" <<'EOF'
#!/bin/sh
case "$2" in
  status) [ -f "$SB/service-started" ] && exit 0 || exit 3 ;;
  start)  : > "$SB/service-started"; exit 0 ;;
  stop)   rm -f "$SB/service-started"; exit 0 ;;
esac
exit 0
EOF
  cat > "$SB/bin-init/rc-update" <<'EOF'
#!/bin/sh
exit 0
EOF
  ;;
systemd)
  cat > "$SB/bin-init/systemctl" <<'EOF'
#!/bin/sh
case "$1" in
  is-active) [ -f "$SB/service-started" ] && exit 0 || exit 3 ;;
  start)     : > "$SB/service-started"; exit 0 ;;
  stop)      rm -f "$SB/service-started"; exit 0 ;;
esac
exit 0
EOF
  ;;
esac
chmod +x "$SB/bin-init/"* 2>/dev/null || true

# The real host always has these init directories; the sandbox must create them.
mkdir -p "$SB/etc/init.d" "$SB/etc/systemd/system" "$SB/etc/meridian-agent" \
         "$SB/opt/meridian-agent" "$SB/var/lib/meridian-agent" "$SB/var/log"

# Sandbox the absolute install paths so the result can be inspected, and
# neutralise the self-restart exec loop (the stub agent does not re-exec).
sed -e 's#/opt/meridian-agent#/tmp/sb/opt/meridian-agent#g' \
    -e 's#/var/lib/meridian-agent#/tmp/sb/var/lib/meridian-agent#g' \
    -e 's#/etc/meridian-agent#/tmp/sb/etc/meridian-agent#g' \
    -e 's#/etc/systemd/system/meridian-agent.service#/tmp/sb/etc/systemd/system/meridian-agent.service#g' \
    -e 's#/etc/init.d/meridian-agent#/tmp/sb/etc/init.d/meridian-agent#g' \
    -e 's#/var/log/meridian-agent.log#/tmp/sb/var/log/meridian-agent.log#g' \
    -e 's#^exec /bin/sh -s --#exec true; : #' \
    /tmp/agent-install.sh > "$SB/agent-install.sh"
chmod +x "$SB/agent-install.sh"

# sha256sum is stubbed to echo this, so the download always "verifies".
printf '%s' 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' > "$SB/expected-sha"

# A real init system starts the unit; the stub service manager cannot, so the
# test pre-seeds an enrolled state. That is also what a reinstall over an
# existing node looks like, and it makes the installer skip the enrollment wait.
mkdir -p "$SB/var/lib/meridian-agent"
printf '%s' '{"node_guid":"e2e","agent_token":"e2e","session_epoch":1}' > "$SB/var/lib/meridian-agent/state.json"

echo "########## scenario: $SCENARIO ##########"
set +e
PATH="$SB/bin-common:$SB/bin-init:/bin:/usr/bin" TMPDIR="$SB/tmp" \
  /bin/sh "$SB/agent-install.sh" -e https://panel.example.test:9090 -t e2e-token \
  > "$SB/install.log" 2>&1
rc=$?
set -e
echo "installer exit code: $rc"
sed 's/^/    /' "$SB/install.log" | tail -n 10

fail=0
# busybox ash aborts a surrounding `set -e` shell when a bare command fails, so
# every probe is wrapped and its status captured explicitly.
probe() { local label="$1"; shift; if "$@" >/dev/null 2>&1; then echo "  PASS  $label"; else echo "  FAIL  $label"; fail=1; fi; }
probe_not() { local label="$1"; shift; if "$@" >/dev/null 2>&1; then echo "  FAIL  $label"; fail=1; else echo "  PASS  $label"; fi; }
expect_rc() { if [ "$rc" = "$1" ]; then echo "  PASS  $2"; else echo "  FAIL  $2 (rc=$rc)"; fail=1; fi; }

case "$SCENARIO" in
openrc)
  UNIT="$SB/etc/init.d/meridian-agent"
  expect_rc 0 "installer succeeded"
  probe "init script created at /etc/init.d/meridian-agent" test -f "$UNIT"
  probe "init script is executable" test -x "$UNIT"
  probe "shebang is openrc-run" grep -q 'openrc-run' "$UNIT"
  probe "uses supervise-daemon" grep -q 'supervise-daemon' "$UNIT"
  probe "respawn is unlimited (self-update safe)" grep -q 'respawn_max=0' "$UNIT"
  probe "respawn delay present" grep -q 'respawn_delay' "$UNIT"
  probe "runs in background" grep -q 'command_background="yes"' "$UNIT"
  probe "command points at the installed agent binary" grep -q "^command=\"$SB/opt/meridian-agent/meridian-agent\"" "$UNIT"
  probe "command_args carries the controller URL" grep -q -- '--controller https://panel.example.test:9090' "$UNIT"
  probe "command_args carries the state file" grep -q -- '--state' "$UNIT"
  probe "generated init script is valid shell" /bin/sh -n "$UNIT"
  probe_not "no systemd unit written" test -e "$SB/etc/systemd/system/meridian-agent.service"
  probe_not "no systemctl in the generated script" grep -q 'systemctl' "$UNIT"
  probe_not "one-time enrollment token consumed" test -e "$SB/etc/meridian-agent/enrollment-token"
  probe "state.json present after install" test -s "$SB/var/lib/meridian-agent/state.json"
  probe "agent binary installed" test -x "$SB/opt/meridian-agent/meridian-agent"
  echo "  --- generated /etc/init.d/meridian-agent ---"
  sed 's/^/  /' "$UNIT"
  ;;
systemd)
  UNIT="$SB/etc/systemd/system/meridian-agent.service"
  expect_rc 0 "installer succeeded"
  probe "systemd unit created" test -f "$UNIT"
  probe "Restart=always preserved" grep -q 'Restart=always' "$UNIT"
  probe "RestartSec=5 preserved" grep -q 'RestartSec=5' "$UNIT"
  probe "hardening directives preserved" grep -q 'ProtectSystem=strict' "$UNIT"
  probe_not "no OpenRC script written" test -e "$SB/etc/init.d/meridian-agent"
  ;;
none)
  if [ "$rc" != "0" ]; then echo "  PASS  installer refused to run"; else echo "  FAIL  installer refused to run"; fail=1; fi
  probe "names both supported inits" grep -q 'supports systemd and OpenRC only' "$SB/install.log"
  probe_not "no misleading systemd-only message" grep -q 'systemd is required' "$SB/install.log"
  ;;
esac

echo
[ "$fail" = "0" ] || { echo "SCENARIO $SCENARIO: FAIL"; exit 1; }
echo "SCENARIO $SCENARIO: PASS"
CONTAINER

OVERALL=0
for scenario in openrc systemd none; do
    echo
    docker run --rm --platform linux/amd64 \
        -v "$WORK/agent-install.sh:/tmp/agent-install.sh:ro" \
        -v "$WORK/harness.sh:/tmp/harness.sh:ro" \
        "$IMAGE" sh /tmp/harness.sh "$scenario" || OVERALL=1
done

echo
if [ "$OVERALL" = "0" ]; then
    echo 'ALL SCENARIOS PASS'
else
    echo 'SOME SCENARIOS FAILED'
fi
exit "$OVERALL"
