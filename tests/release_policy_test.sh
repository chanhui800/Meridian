#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY="${ROOT_DIR}/scripts/release-policy.sh"

for valid in v1.9.38 v1.10.0 v2.0.0 v10.0.0; do
  bash "$POLICY" is-valid "$valid"
done
for invalid in 1.9.38 v1.9 v1.9.38-beta latest main v01.9.38; do
  if bash "$POLICY" is-valid "$invalid"; then
    echo "invalid release tag accepted: $invalid" >&2
    exit 1
  fi
done

highest="$(bash "$POLICY" highest v1.9.37 v1.9.38 v1.9.9)"
[ "$highest" = "v1.9.38" ]
highest="$(bash "$POLICY" highest v1.9.99 v1.10.0 v2.0.0 v10.0.0)"
[ "$highest" = "v10.0.0" ]

for pair in \
  "v1.9.38 v1.9.38 yes" \
  "v1.9.37 v1.9.38 no" \
  "v1.10.0 v1.9.99 yes" \
  "v2.0.0 v1.99.99 yes" \
  "v10.0.0 v2.999.999 yes"; do
  read -r current latest expected <<< "$pair"
  actual=no
  if bash "$POLICY" should-promote "$current" "$latest"; then actual=yes; fi
  [ "$actual" = "$expected" ] || { echo "promotion mismatch: $pair -> $actual" >&2; exit 1; }
done

echo "release policy tests passed"
