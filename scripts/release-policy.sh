#!/usr/bin/env bash
set -euo pipefail

# Stable three-part SemVer release tags only. Pre-releases are intentionally
# rejected until they have an explicit promotion policy.
is_valid_release_tag() {
  [[ "${1:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

version_key() {
  local value="${1#v}"
  IFS='.' read -r major minor patch <<< "$value"
  printf '%020d.%020d.%020d\n' "$major" "$minor" "$patch"
}

version_gt() {
  is_valid_release_tag "$1" && is_valid_release_tag "$2" || return 1
  [[ "$(version_key "$1")" > "$(version_key "$2")" ]]
}

highest_release_tag() {
  local candidate highest=""
  for candidate in "$@"; do
    if ! is_valid_release_tag "$candidate"; then continue; fi
    if [ -z "$highest" ] || version_gt "$candidate" "$highest"; then highest="$candidate"; fi
  done
  [ -n "$highest" ] || return 1
  printf '%s\n' "$highest"
}

should_promote_latest() {
  local current="${1:-}" highest="${2:-}"
  is_valid_release_tag "$current" || return 1
  if [ -z "$highest" ]; then return 0; fi
  is_valid_release_tag "$highest" || return 1
  ! version_gt "$highest" "$current"
}

usage() { echo "usage: $0 {is-valid|highest|should-promote} ..." >&2; exit 2; }

case "${1:-}" in
  is-valid)
    [ "$#" -eq 2 ] || usage
    is_valid_release_tag "$2"
    ;;
  highest)
    shift
    if [ "$#" -gt 0 ]; then
      highest_release_tag "$@"
    else
      candidates=()
      while IFS= read -r candidate; do candidates+=("$candidate"); done
      [ "${#candidates[@]}" -gt 0 ] || usage
      highest_release_tag "${candidates[@]}"
    fi
    ;;
  should-promote)
    [ "$#" -ge 2 ] && [ "$#" -le 3 ] || usage
    should_promote_latest "$2" "${3:-}"
    ;;
  *) usage ;;
esac
