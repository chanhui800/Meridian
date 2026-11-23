#!/usr/bin/env bash
# shellcheck disable=SC2317,SC2329
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# The override permits replaying this exact regression against an old source.
# shellcheck disable=SC1090
source "${MERIDIAN_INSTALLER_TEST_SOURCE:-${REPO_ROOT}/install.sh}"

DATA_DIR=/srv/meridian-health-test
INSTALL_DIR=/srv/meridian-health-binary
health_db_value=/srv/meridian-health-test/custom.db
health_calls=0
health_failures=0
health_parameters=()
read_env_value() {
    if [ "$1" = DB_PATH ]; then printf '%s\n' "$health_db_value";
    elif [ "$1" = PORT ]; then printf '9090\n'; fi
}
sleep() { :; }
curl() { return 1; } # No real network command can leave this wrapper fixture.
as_root() {
    health_calls=$((health_calls + 1))
    health_parameters=("$@")
    if [ "$health_calls" -le "$health_failures" ]; then return 1; fi
    [ "$#" = 4 ] && [ "$1" = env ] && [ "$3" = "$INSTALL_DIR/$BIN_NAME" ] && [ "$4" = --healthcheck ]
}
fail_test() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

for health_db_value in /srv/meridian-health-test/custom.db '"/srv/meridian-health-test/custom.db"' "'/srv/meridian-health-test/custom.db'"; do
    health_calls=0
    wait_for_health 1 || fail_test 'healthy application was rejected instead of using its actual listener healthcheck'
    [ "$health_calls" = 1 ] || fail_test 'application healthcheck was not called exactly once'
    [ "${health_parameters[1]}" = DB_PATH=/srv/meridian-health-test/custom.db ] || fail_test 'database configuration was not passed to the healthcheck'
done
printf 'PASS actual application healthcheck receives the configured database path, including quotes\n'

health_db_value=''; health_calls=0
wait_for_health 1 || fail_test 'missing DB_PATH did not use the native installation default'
[ "${health_parameters[1]}" = "DB_PATH=$DATA_DIR/meridian.db" ] || fail_test 'healthcheck used the Docker database default'
printf 'PASS absent database path uses the native data directory\n'

health_db_value=/srv/meridian-health-test/custom.db
health_calls=0; health_failures=1
wait_for_health 2 || fail_test 'transient application startup did not recover'
[ "$health_calls" = 2 ] || fail_test 'healthcheck retries did not call the application again'
health_calls=0; health_failures=3
if wait_for_health 2; then fail_test 'persistent unhealthy application was accepted'; fi
[ "$health_calls" = 2 ] || fail_test 'healthcheck ignored its retry bound'
printf 'PASS startup retries remain bounded and persistent failure is refused\n'

health_db_value='relative.db'; health_calls=0; health_failures=0
if wait_for_health 1; then fail_test 'unsafe database path was accepted'; fi
[ "$health_calls" = 0 ] || fail_test 'healthcheck ran before rejecting its database path'
printf 'installer health tests passed (no configuration executed or network called)\n'
