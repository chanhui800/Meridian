#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT

export MERIDIAN_DATA_DIR="${TEST_ROOT}/data"
mkdir -p "$MERIDIAN_DATA_DIR"

# shellcheck disable=SC1091
source "${REPO_ROOT}/install.sh"

as_root() { command "$@"; }
is_systemd() { return 1; }
install_env_file() {
    cp "$1" "$(env_file_path)"
    chmod 0600 "$(env_file_path)"
}

repeat_secret_byte() {
    printf '%*s' 64 '' | tr ' ' "$1"
}

# Persist the sequence outside command substitutions. Deliberate collisions in
# the first six calls prove fresh provisioning retries rather than reusing a
# JWT or upstream key.
GENERATOR_STATE="${TEST_ROOT}/generator-state"
generate_secret() {
    local current=0 next
    [ ! -f "$GENERATOR_STATE" ] || current=$(cat "$GENERATOR_STATE")
    next=$((current + 1))
    printf '%s\n' "$next" > "$GENERATOR_STATE"
    case "$next" in
        1|2|4) repeat_secret_byte a ;;
        3|5) repeat_secret_byte b ;;
        6) repeat_secret_byte c ;;
        7) repeat_secret_byte d ;;
        *) printf '%064d\n' "$next" ;;
    esac
}

ENV_FILE=$(env_file_path)
work_dir="${TEST_ROOT}/work"
mkdir -p "$work_dir"

fail_test() {
    printf 'FAIL: %s\n' "$1" >&2
    exit 1
}

assert_rejected_unchanged() {
    local label="$1"
    local before="${TEST_ROOT}/${label}.before"
    local log="${TEST_ROOT}/${label}.log"
    cp "$ENV_FILE" "$before"
    if (ensure_dynamic_route_key "$work_dir") >"$log" 2>&1; then
        fail_test "${label} secret configuration was accepted"
    fi
    cmp -s "$ENV_FILE" "$before" \
        || fail_test "rejected ${label} configuration changed .env"
}

# Fresh configuration retries deliberate generator collisions, provisions four
# distinct values, and never writes any of them to installer output.
prepare_log="${TEST_ROOT}/prepare.log"
prepare_data_and_config "$work_dir" >"$prepare_log" 2>&1
fresh_jwt=$(read_env_value JWT_SECRET)
fresh_upstream=$(read_env_value UPSTREAM_HEADER_KEY)
fresh_dynamic=$(read_env_value DYNAMIC_ROUTE_KEY)
fresh_setup=$(read_env_value SETUP_TOKEN)
for secret in "$fresh_jwt" "$fresh_upstream" "$fresh_dynamic" "$fresh_setup"; do
    [ "${#secret}" -ge 32 ] || fail_test 'fresh configuration generated a short secret'
    if grep -Fq -- "$secret" "$prepare_log"; then
        fail_test 'fresh configuration printed a generated secret'
    fi
done
[ "$fresh_jwt" != "$fresh_upstream" ] || fail_test 'fresh JWT and upstream key are equal'
[ "$fresh_jwt" != "$fresh_dynamic" ] || fail_test 'fresh JWT and dynamic key are equal'
[ "$fresh_upstream" != "$fresh_dynamic" ] || fail_test 'fresh upstream and dynamic keys are equal'
if [ "$fresh_setup" = "$fresh_jwt" ] || [ "$fresh_setup" = "$fresh_upstream" ] \
    || [ "$fresh_setup" = "$fresh_dynamic" ]; then
    fail_test 'fresh setup token was not generated distinctly'
fi

jwt_key=$(repeat_secret_byte j)
upstream_key=$(repeat_secret_byte u)
valid_key=$(repeat_secret_byte v)

# A valid existing value and every other byte in .env remain untouched.
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$valid_key" > "$ENV_FILE"
cp "$ENV_FILE" "${TEST_ROOT}/valid.before"
ensure_dynamic_route_key "$work_dir"
cmp -s "$ENV_FILE" "${TEST_ROOT}/valid.before" \
    || fail_test 'valid DYNAMIC_ROUTE_KEY was not preserved byte-exactly'

# Existing v1.7 EnvironmentFile secrets may use one simple pair of systemd
# quotes. Backfill compares their effective values while retaining every old
# byte, including the quoted assignment lines and unrelated records.
legacy_jwt=$(printf '%32s' '' | tr ' ' J)
legacy_upstream=$(printf '%32s' '' | tr ' ' U)
{
    printf '%s\n' '# preserved v1.7 comment = "literal bytes"'
    printf 'JWT_SECRET="%s"\n' "$legacy_jwt"
    printf '%s\n' 'UNRELATED_SETTING=keep-this-byte-for-byte'
    printf "UPSTREAM_HEADER_KEY='%s'\n" "$legacy_upstream"
    printf 'PORT=9090'
} > "$ENV_FILE"
cp "$ENV_FILE" "${TEST_ROOT}/quoted-legacy.before"
ensure_dynamic_route_key "$work_dir"
quoted_dynamic=$(read_env_value DYNAMIC_ROUTE_KEY)
[ "$(read_legacy_env_secret JWT_SECRET)" = "$legacy_jwt" ] \
    || fail_test 'double-quoted JWT effective value was parsed incorrectly'
[ "$(read_legacy_env_secret UPSTREAM_HEADER_KEY)" = "$legacy_upstream" ] \
    || fail_test 'single-quoted upstream key effective value was parsed incorrectly'
if [ "$quoted_dynamic" = "$legacy_jwt" ] || [ "$quoted_dynamic" = "$legacy_upstream" ]; then
    fail_test 'quoted legacy backfill reused an effective legacy secret'
fi
cp "${TEST_ROOT}/quoted-legacy.before" "${TEST_ROOT}/quoted-legacy.expected"
printf '\nDYNAMIC_ROUTE_KEY=%s\n' "$quoted_dynamic" >> "${TEST_ROOT}/quoted-legacy.expected"
cmp -s "$ENV_FILE" "${TEST_ROOT}/quoted-legacy.expected" \
    || fail_test 'quoted legacy backfill did not preserve all prior bytes'
grep -Fqx -- "JWT_SECRET=\"${legacy_jwt}\"" "$ENV_FILE" \
    || fail_test 'quoted JWT line was rewritten'
grep -Fqx -- "UPSTREAM_HEADER_KEY='${legacy_upstream}'" "$ENV_FILE" \
    || fail_test 'quoted upstream key line was rewritten'

# Equality uses effective unquoted legacy values, not their lexical quoted
# representation, and rejection leaves the complete file byte-identical.
{
    printf 'JWT_SECRET="%s"\n' "$legacy_jwt"
    printf "UPSTREAM_HEADER_KEY='%s'\n" "$legacy_upstream"
    printf 'DYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' "$legacy_jwt"
} > "$ENV_FILE"
assert_rejected_unchanged effective-equal-quoted-jwt

{
    printf 'JWT_SECRET="%s"\n' "$legacy_jwt"
    printf "UPSTREAM_HEADER_KEY='%s'\n" "$legacy_upstream"
    printf 'DYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' "$legacy_upstream"
} > "$ENV_FILE"
assert_rejected_unchanged effective-equal-quoted-upstream

{
    printf 'JWT_SECRET="%s"\n' "$legacy_jwt"
    printf "UPSTREAM_HEADER_KEY='%s'\nPORT=9090\n" "$legacy_jwt"
} > "$ENV_FILE"
assert_rejected_unchanged effective-equal-quoted-legacy

# Quote bytes do not count toward the minimum effective secret length, and
# unmatched or duplicate legacy assignments remain fail-closed.
short_legacy=$(printf '%31s' '' | tr ' ' s)
{
    printf 'JWT_SECRET="%s"\n' "$short_legacy"
    printf "UPSTREAM_HEADER_KEY='%s'\nPORT=9090\n" "$legacy_upstream"
} > "$ENV_FILE"
assert_rejected_unchanged short-effective-quoted-jwt

{
    printf 'JWT_SECRET="%s"\n' "$legacy_jwt"
    printf "UPSTREAM_HEADER_KEY='%s'\nPORT=9090\n" "$short_legacy"
} > "$ENV_FILE"
assert_rejected_unchanged short-effective-quoted-upstream

{
    printf 'JWT_SECRET="%s\n' "$legacy_jwt"
    printf "UPSTREAM_HEADER_KEY='%s'\nPORT=9090\n" "$legacy_upstream"
} > "$ENV_FILE"
assert_rejected_unchanged unmatched-legacy-quote

{
    printf 'JWT_SECRET="%s"\nJWT_SECRET=%s\n' "$legacy_jwt" "$legacy_jwt"
    printf "UPSTREAM_HEADER_KEY='%s'\nPORT=9090\n" "$legacy_upstream"
} > "$ENV_FILE"
assert_rejected_unchanged duplicate-legacy-secret

# Explicitly empty and missing legacy definitions are backfilled once with a
# value distinct from both effective existing keys.
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" > "$ENV_FILE"
ensure_dynamic_route_key "$work_dir"
repaired_key=$(read_env_value DYNAMIC_ROUTE_KEY)
[ "${#repaired_key}" -ge 32 ] || fail_test 'empty DYNAMIC_ROUTE_KEY was not repaired'
if [ "$repaired_key" = "$jwt_key" ] || [ "$repaired_key" = "$upstream_key" ]; then
    fail_test 'empty-key repair reused another secret'
fi
[ "$(grep -c '^DYNAMIC_ROUTE_KEY=' "$ENV_FILE")" = "1" ] \
    || fail_test 'empty repair did not leave exactly one DYNAMIC_ROUTE_KEY'

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" > "$ENV_FILE"
ensure_dynamic_route_key "$work_dir"
backfilled_key=$(read_env_value DYNAMIC_ROUTE_KEY)
[ "${#backfilled_key}" -ge 32 ] || fail_test 'missing DYNAMIC_ROUTE_KEY was not backfilled'
if [ "$backfilled_key" = "$jwt_key" ] || [ "$backfilled_key" = "$upstream_key" ]; then
    fail_test 'missing-key backfill reused another secret'
fi
[ "$(grep -c '^DYNAMIC_ROUTE_KEY=' "$ENV_FILE")" = "1" ] \
    || fail_test 'missing-key backfill did not add exactly one DYNAMIC_ROUTE_KEY'

# Invalid effective EnvironmentFile forms fail before any rewrite. Quoted and
# escaped values are long enough lexically but systemd would load other bytes.
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=too-short\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" > "$ENV_FILE"
assert_rejected_unchanged short

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY= \t \nPORT=9090\n' \
    "$jwt_key" "$upstream_key" > "$ENV_FILE"
assert_rejected_unchanged whitespace-only

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s value\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$valid_key" > "$ENV_FILE"
assert_rejected_unchanged ascii-whitespace

quoted_payload=$(printf '%31s' '' | tr ' ' q)
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY="%s"\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$quoted_payload" > "$ENV_FILE"
assert_rejected_unchanged double-quoted

printf "JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY='%s'\nPORT=9090\n" \
    "$jwt_key" "$upstream_key" "$quoted_payload" > "$ENV_FILE"
assert_rejected_unchanged single-quoted

escaped_payload=$(printf '%31s' '' | tr ' ' e)
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s\\x\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$escaped_payload" > "$ENV_FILE"
assert_rejected_unchanged escaped

unicode_space=$'\u00a0'
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s%sx\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$valid_key" "$unicode_space" > "$ENV_FILE"
assert_rejected_unchanged unicode-whitespace

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\n export DYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$valid_key" > "$ENV_FILE"
assert_rejected_unchanged exported-ambiguous

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY =%s\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$valid_key" > "$ENV_FILE"
assert_rejected_unchanged spaced-assignment

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s\nDYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' \
    "$jwt_key" "$upstream_key" "$valid_key" "$valid_key" > "$ENV_FILE"
assert_rejected_unchanged duplicate

# Equality remains byte-for-byte after legacy quote decoding and strict dynamic parsing.
printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' \
    "$valid_key" "$upstream_key" "$valid_key" > "$ENV_FILE"
assert_rejected_unchanged equal-jwt

printf 'JWT_SECRET=%s\nUPSTREAM_HEADER_KEY=%s\nDYNAMIC_ROUTE_KEY=%s\nPORT=9090\n' \
    "$jwt_key" "$valid_key" "$valid_key" > "$ENV_FILE"
assert_rejected_unchanged equal-upstream

echo 'dynamic route key tests passed'
