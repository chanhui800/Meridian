#!/usr/bin/env bash
# Structure tests for scripts/agent-install.sh init-system support.
#
# The Agent is supervised by the system init and exits(1) after replacing its
# own binary (self-update), so the unit the installer writes MUST respawn. A
# non-respawning unit would leave a node permanently offline after its first
# self-update, which is the failure these assertions exist to prevent.
#
# This file guards the script's structure. Behavioural verification runs the real
# installer inside an Alpine container with stubbed network/service commands; see
# the pull request that added OpenRC support for that harness.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="${REPO_ROOT}/scripts/agent-install.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# assert_present <needle> <description>
assert_present() {
    grep -Fq -- "$1" "$SCRIPT" || fail "$2 (missing: $1)"
}

# assert_absent <needle> <description>
assert_absent() {
    if grep -Fq -- "$1" "$SCRIPT"; then
        fail "$2 (unexpected: $1)"
    fi
}

echo '=== agent-install.sh: init system structure ==='

echo '[1] init detection covers systemd and OpenRC, and names both on refusal'
assert_present "init_system='systemd'" 'systemd is not detected'
assert_present "init_system='openrc'" 'OpenRC is not detected'
assert_present 'command -v openrc' 'OpenRC is not probed by command name'
assert_present 'command -v rc-service' 'rc-service is not probed'
assert_present 'supports systemd and OpenRC only' 'the refusal does not name both init systems'
assert_present 'Detected init:' 'the refusal does not report the detected init'

echo '[2] the old misleading systemd-only refusal is gone'
assert_absent 'systemd is required' 'the systemd-only message is still present'

echo '[3] systemctl is only reachable through the service helper layer'
stray=$(grep -n 'systemctl' "$SCRIPT" | grep -v 'command -v systemctl' | grep -v 'systemd) systemctl' || true)
if [ -n "$stray" ]; then
    printf '%s\n' "$stray" >&2
    fail 'systemctl is called outside the cmd_service_* helper layer'
fi

echo '[4] the service helper layer covers every lifecycle operation'
for helper in cmd_service_stop cmd_service_enable cmd_service_start cmd_service_active \
              cmd_service_enabled cmd_service_reload cmd_service_install; do
    assert_present "${helper}()" "missing helper: ${helper}"
done
# Each helper must have an OpenRC arm, otherwise an Alpine host runs the systemd
# branch (or nothing).
openrc_arms=$(grep -c 'openrc) ' "$SCRIPT")
if [ "$openrc_arms" -lt 7 ]; then
    fail "expected at least 7 OpenRC arms in the helper layer, found ${openrc_arms}"
fi

echo '[5] the rendered OpenRC unit is a supervised, respawning service'
assert_present '#!/sbin/openrc-run' 'the OpenRC unit is missing its interpreter'
assert_present 'supervisor="supervise-daemon"' 'the OpenRC unit is not supervised'
# The Agent exits non-zero on purpose after a self-update; only an unlimited
# respawn keeps restarting it.
assert_present 'respawn_max=0' 'the OpenRC unit does not respawn forever'
assert_present 'respawn_delay=' 'the OpenRC unit has no respawn delay'
assert_present 'command_background="yes"' 'the OpenRC unit does not run in the background'
assert_present 'command_user="root"' 'the OpenRC unit does not pin its user'
assert_present 'rc_ulimit=' 'the OpenRC unit has no fd limit'
assert_present 'depend()' 'the OpenRC unit declares no dependencies'

echo '[6] the resolved paths reach the unit that gets installed'
assert_present 'openrc_service_file=/etc/init.d/meridian-agent' 'the OpenRC unit path is not the init.d location'
assert_present 'managed_service_file=' 'the rollback path does not track the installed unit'

echo '[7] the systemd unit keeps its restart and hardening contract'
assert_present 'Restart=always' 'the systemd unit lost Restart=always'
assert_present 'RestartSec=5' 'the systemd unit lost RestartSec'
assert_present 'NoNewPrivileges=true' 'the systemd unit lost NoNewPrivileges'
assert_present 'ProtectSystem=strict' 'the systemd unit lost ProtectSystem=strict'
assert_present 'WantedBy=multi-user.target' 'the systemd unit lost its install target'

echo 'agent installer structure tests passed'
