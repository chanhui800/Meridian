#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
image="meridian-entrypoint-test-$$"
test_root=$(mktemp -d)

cleanup() {
    docker image rm -f "$image" >/dev/null 2>&1 || true
    if [ -d "$test_root" ]; then
        docker run --rm -v "$test_root:/cleanup" alpine:3.24 \
            sh -c 'chmod -R 0777 /cleanup' >/dev/null 2>&1 || true
        rm -rf -- "$test_root"
    fi
}
trap cleanup EXIT INT TERM

fail_test() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

read_secret() {
    local data_dir=$1 name=$2
    docker run --rm --entrypoint sh -v "$data_dir:/data:ro" "$image" -c \
        'line=$(grep -m1 "^${1}=" /data/.meridian-secrets) || exit 1; printf "%s\n" "${line#*=}"' \
        sh "$name"
}

docker build --build-arg VERSION=v1.8.26 -t "$image" "$repo_root" >/dev/null

binary_capability=$(docker run --rm --entrypoint sh "$image" -c 'getcap /app/meridian')
printf '%s\n' "$binary_capability" | grep -Fq 'cap_net_bind_service=ep' \
    || fail_test "Meridian binary lacks CAP_NET_BIND_SERVICE: $binary_capability"

fresh_data="$test_root/fresh"
mkdir -p "$fresh_data"
first_output=$(docker run --rm -v "$fresh_data:/app/data" "$image" --version)
printf '%s\n' "$first_output" | grep -Fq 'v1.8.26' \
    || fail_test '--version was not forwarded to Meridian'
printf '%s\n' "$first_output" | grep -Fq 'Meridian 首次初始化令牌:' \
    || fail_test 'the generated setup token was not printed on first startup'

declare -a names=(JWT_SECRET UPSTREAM_HEADER_KEY DYNAMIC_ROUTE_KEY SETUP_TOKEN)
declare -a values=()
for name in "${names[@]}"; do
    value=$(read_secret "$fresh_data" "$name")
    [ "${#value}" -ge 32 ] || fail_test "$name is shorter than 32 bytes"
    values+=("$value")
done
for ((i = 0; i < ${#values[@]}; i++)); do
    for ((j = i + 1; j < ${#values[@]}; j++)); do
        [ "${values[$i]}" != "${values[$j]}" ] \
            || fail_test "${names[$i]} and ${names[$j]} are identical"
    done
done

setup_from_log=$(printf '%s\n' "$first_output" | sed -n 's/^Meridian 首次初始化令牌: //p')
[ "$setup_from_log" = "${values[3]}" ] \
    || fail_test 'the logged setup token does not match the persisted token'

mode_owner=$(docker run --rm --entrypoint sh -v "$fresh_data:/data:ro" "$image" -c \
    "stat -c '%a:%u' /data/.meridian-secrets")
[ "$mode_owner" = '600:10001' ] \
    || fail_test "secrets file mode/owner is $mode_owner, want 600:10001"
data_owner=$(docker run --rm --entrypoint sh -v "$fresh_data:/data:ro" "$image" -c \
    "stat -c '%u' /data")
[ "$data_owner" = '10001' ] \
    || fail_test "data directory owner is $data_owner, want 10001"

second_output=$(docker run --rm -v "$fresh_data:/app/data" "$image" --version)
if printf '%s\n' "$second_output" | grep -Fq 'Meridian 首次初始化令牌:'; then
    fail_test 'the setup token was printed again after it had already been persisted'
fi
for i in "${!names[@]}"; do
    [ "$(read_secret "$fresh_data" "${names[$i]}")" = "${values[$i]}" ] \
        || fail_test "${names[$i]} changed after restart"
done

explicit_data="$test_root/explicit"
mkdir -p "$explicit_data"
explicit_jwt=jwt-explicit-000000000000000000000000000000000001
explicit_upstream=upstream-explicit-00000000000000000000000000000002
explicit_dynamic=dynamic-explicit-000000000000000000000000000000003
explicit_setup=setup-explicit-0000000000000000000000000000000004
docker run --rm -v "$explicit_data:/app/data" \
    -e "JWT_SECRET=$explicit_jwt" \
    -e "UPSTREAM_HEADER_KEY=$explicit_upstream" \
    -e "DYNAMIC_ROUTE_KEY=$explicit_dynamic" \
    -e "SETUP_TOKEN=$explicit_setup" \
    "$image" --version >/dev/null
for pair in \
    "JWT_SECRET:$explicit_jwt" \
    "UPSTREAM_HEADER_KEY:$explicit_upstream" \
    "DYNAMIC_ROUTE_KEY:$explicit_dynamic" \
    "SETUP_TOKEN:$explicit_setup"; do
    name=${pair%%:*}
    expected=${pair#*:}
    [ "$(read_secret "$explicit_data" "$name")" = "$expected" ] \
        || fail_test "$name did not honor the explicit environment override"
done

shell_data="$test_root/shell"
mkdir -p "$shell_data"
docker run --rm -v "$shell_data:/app/data" "$image" sh -c \
    'test ! -e /app/data/.meridian-secrets' \
    || fail_test 'an explicit shell command unexpectedly initialized application data'

admin_data="$test_root/admin"
mkdir -p "$admin_data"
if docker run --rm -v "$admin_data:/app/data" "$image" admin >"$test_root/admin.out" 2>&1; then
    fail_test 'an incomplete admin command unexpectedly succeeded'
fi
grep -Fq 'usage: meridian admin reset-password' "$test_root/admin.out" \
    || fail_test 'the admin command was not forwarded to Meridian'

printf 'docker entrypoint tests passed\n'
