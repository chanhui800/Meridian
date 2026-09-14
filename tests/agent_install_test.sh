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

echo '[8] a node enrolled to another Controller is reported, not silently skipped'
# The stored credential must be checked against the Controller before the
# installer concludes the node is already enrolled: an install that skips
# enrollment yet prints success leaves the panel waiting forever.
assert_present 'agent_enrolled_with_controller()' 'the installer does not verify the stored credential'
assert_present 'state_agent_token()' 'the installer does not read the stored Agent token'
assert_present '/api/agent/config' 'the credential probe does not target the config endpoint'
assert_present '401|403' 'the probe does not treat an auth rejection as "another Controller"'
assert_present 'already enrolled to a different Meridian Controller' 'the refusal does not say what is wrong'
assert_present 'keep showing it as pending' 'the refusal does not explain the visible symptom'
assert_present "'<NEW_ENROLLMENT_TOKEN>' --reenroll" 'the refusal does not hand over a runnable --reenroll command'
# Guard the ordering: the credential probe must run before the service starts, so
# a refused install never leaves an Agent running against the wrong Controller.
probe_line=$(grep -n 'agent_enrolled_with_controller;' "$SCRIPT" | head -n 1 | cut -d: -f1)
start_line=$(grep -n 'if ! cmd_service_start' "$SCRIPT" | head -n 1 | cut -d: -f1)
if [ -z "$probe_line" ] || [ -z "$start_line" ] || [ "$probe_line" -ge "$start_line" ]; then
    fail "the credential probe must run before the service is started (probe=$probe_line start=$start_line)"
fi

echo 'agent installer structure tests passed'
