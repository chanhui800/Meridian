#!/usr/bin/env bash
# shellcheck disable=SC2034,SC2317,SC2329
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck disable=SC1091
source "${REPO_ROOT}/install.sh"

# All filesystem, privilege, service, and account operations are virtual. The
# real uninstall entry point may only record removal arguments in this harness.
init_privilege() { ROOT_PREFIX=(); }
is_systemd() { [ "$audit_systemd" -eq 1 ]; }
id() { return 1; }
getent() { return 1; }
audit_removals=()
audit_recursive=()
audit_marker_present=0
audit_env_present=0
audit_unit_present=0
audit_data_present=1
audit_final_symlink=0
audit_marker_symlink=0
audit_marker_owner=0
audit_marker_mode=600
audit_marker_text='meridian-data-directory-v1'
audit_env_text=''
audit_resolved=''
audit_empty_data=0
audit_env_mode=600
audit_unit_owner=0
audit_marker_creations=0
audit_marker_command=''
audit_changed_after_removal=''
audit_systemd=0

# Model the installer's direct unit-file existence test as well as as_root's
# tests; there is no physical unit or service in this virtual filesystem.
function [ {
  if builtin [ "$#" -eq 3 ] && builtin [ "$1" = -f ] && builtin [ "$2" = "$SERVICE_FILE" ]; then
    builtin [ "$audit_unit_present" -eq 1 ]
  else
    builtin [ "$@"
  fi
}

as_root() {
  local operation="$1" target last_index input_text
  shift
  target="${!#}"
  case "$operation" in
    test)
      case "$1:$target" in
        "-e:$NGINX_CONFIG"|"-L:$NGINX_CONFIG") return 1 ;;
        "-L:$DATA_DIR") [ "$audit_final_symlink" -eq 1 ] ;;
        -L:*/.meridian-data-directory) [ "$audit_marker_symlink" -eq 1 ] ;;
        -e:*/.meridian-data-directory|-f:*/.meridian-data-directory) [ "$audit_marker_present" -eq 1 ] ;;
        -e:*/.env|-f:*/.env) [ "$audit_env_present" -eq 1 ] ;;
        "-f:$SERVICE_FILE"|"-e:$SERVICE_FILE") [ "$audit_unit_present" -eq 1 ] ;;
        "-e:$DATA_DIR"|"-d:$DATA_DIR") [ "$audit_data_present" -eq 1 ] ;;
        -L:*) return 1 ;;
        -d:*) return 0 ;;
        *) return 1 ;;
      esac
      ;;
    bash)
      # The production resolver passes an existing directory as the final
      # argument to a fixed cd -P script. No shell command is executed here.
      if [ "$target" = 'meridian-data-directory-v1' ]; then
        audit_marker_creations=$((audit_marker_creations + 1))
        audit_marker_command="$*"
        audit_marker_present=1
      elif [ "$target" = "$DATA_DIR" ] && [ -n "$audit_changed_after_removal" ] && [ "${#audit_removals[@]}" -gt 0 ]; then printf '%s\n' "$audit_changed_after_removal"
      elif [ "$target" = "$DATA_DIR" ]; then printf '%s\n' "${audit_resolved:-$DATA_DIR}"
      else printf '%s\n' "$target"
      fi
      ;;
    stat)
      if [[ "$target" = */.meridian-data-directory ]]; then printf '%s:%s\n' "$audit_marker_owner" "$audit_marker_mode"
      elif [[ "$target" = */.env ]]; then printf '0:%s\n' "$audit_env_mode"
      elif [ "$target" = "$SERVICE_FILE" ]; then printf '%s:600\n' "$audit_unit_owner"
      else printf '0:600\n'
      fi
      ;;
    cat)
      if [[ "$target" = */.meridian-data-directory ]]; then printf '%s\n' "$audit_marker_text"; else return 1; fi
      ;;
    awk|grep)
      local arguments=("$@")
      last_index=$((${#arguments[@]} - 1))
      arguments[last_index]='-'
      if [ "$target" = "$SERVICE_FILE" ]; then input_text="WorkingDirectory=$DATA_DIR"$'\n'"ExecStart=$INSTALL_DIR/$BIN_NAME"
      elif [[ "$target" = */.meridian-data-directory ]]; then input_text="$audit_marker_text"
      else input_text="$audit_env_text"
      fi
      printf '%s\n' "$input_text" | command "$operation" "${arguments[@]}"
      ;;
    find) if [ "$audit_empty_data" -eq 0 ]; then printf 'foreign-file\n'; fi ;;
    rm)
      audit_removals+=("$*")
      if [ "$1" = -rf ]; then audit_recursive+=("$target"); fi
      if [ "$target" = "$SERVICE_FILE" ]; then audit_unit_present=0; fi
      ;;
    systemctl)
      if [ "$1" = show ]; then
        printf 'ActiveState=inactive\nMainPID=0\nControlPID=0\n'
      fi
      return 0
      ;;
    *) printf 'Unexpected privileged operation: %s\n' "$operation" >&2; return 1 ;;
  esac
}

reset_audit() {
  audit_removals=(); audit_recursive=()
  audit_marker_present=0; audit_env_present=0; audit_unit_present=0
  audit_data_present=1; audit_final_symlink=0; audit_marker_symlink=0
  audit_marker_owner=0; audit_marker_mode=600
  audit_marker_text='meridian-data-directory-v1'; audit_env_text=''; audit_resolved=''
  audit_empty_data=0; audit_env_mode=600; audit_unit_owner=0
  audit_marker_creations=0; audit_marker_command=''; audit_changed_after_removal=''
  audit_systemd=0
  ASSUME_YES=1; PURGE_DATA=1
}

expect_purge_refused() {
  local label="$1" status=0
  # Run in a subshell because fail() exits. A refusal must happen before even
  # named binaries are removed; no removal command can leave the capture stub.
  if (
    trap 'if [ "${#audit_removals[@]}" -gt 0 ]; then exit 79; fi' EXIT
    do_uninstall >/dev/null 2>&1
  ); then status=0; else status=$?; fi
  if [ "$status" -eq 0 ] || [ "$status" -eq 79 ]; then
    printf 'FAIL: %s was not refused before removal (status=%s)\n' "$label" "$status" >&2
    exit 1
  fi
}

for audit_path in /var/lib /var/log /usr/share /usr/lib /usr/local/bin /lib /lib64; do
  reset_audit
  DATA_DIR="$audit_path"
  expect_purge_refused "system directory $audit_path"
done

reset_audit
DATA_DIR=/srv/meridian-data
audit_resolved=/var/lib
expect_purge_refused 'an ancestor alias resolving into a system directory'

reset_audit
DATA_DIR=/srv/meridian-data
audit_final_symlink=1
audit_marker_present=1
expect_purge_refused 'a terminal data-directory symlink'

reset_audit
DATA_DIR=/srv/foreign-data
expect_purge_refused 'an unmarked foreign directory'

for audit_bad_marker in writable wrong-owner symlink wrong-format; do
  reset_audit
  DATA_DIR=/srv/meridian-data
  audit_marker_present=1
  case "$audit_bad_marker" in
    writable) audit_marker_mode=666 ;;
    wrong-owner) audit_marker_owner=1000 ;;
    symlink) audit_marker_symlink=1 ;;
    wrong-format) audit_marker_text='another application' ;;
  esac
  expect_purge_refused "untrusted marker: $audit_bad_marker"
done

reset_audit
DATA_DIR=/srv/meridian-data
audit_marker_present=1
do_uninstall >/dev/null
[ "${#audit_recursive[@]}" -eq 1 ] && [ "${audit_recursive[0]}" = "$DATA_DIR" ] \
  || { echo 'FAIL: marked dedicated directory was not the sole recursive target' >&2; exit 1; }

reset_audit
DATA_DIR=/srv/legacy-meridian
audit_env_present=1
audit_env_text="JWT_SECRET=synthetic-legacy-jwt-secret-000000000000"$'\n'"PORT=9090"$'\n'"DB_PATH=$DATA_DIR/meridian.db"
do_uninstall >/dev/null
[ "${#audit_recursive[@]}" -eq 1 ] && [ "${audit_recursive[0]}" = "$DATA_DIR" ] \
  || { echo 'FAIL: legitimate legacy native directory was not accepted' >&2; exit 1; }

reset_audit
DATA_DIR=/srv/foreign-app
audit_env_present=1
audit_env_text="JWT_SECRET=synthetic-foreign-jwt-secret-0000000000"$'\n'"PORT=9090"$'\n'"DB_PATH=$DATA_DIR/foreign.db"
expect_purge_refused 'a foreign application with generic environment fields'

reset_audit
DATA_DIR=/srv/legacy-external-db
audit_env_present=1; audit_unit_present=1; audit_systemd=1
audit_env_text="JWT_SECRET=synthetic-legacy-jwt-secret-000000000000"$'\n'"PORT=9090"$'\n'"DB_PATH=/srv/separate-store/media.sqlite"
do_uninstall >/dev/null
[ "${#audit_recursive[@]}" -eq 1 ] && [ "${audit_recursive[0]}" = "$DATA_DIR" ] \
  || { echo 'FAIL: installer-managed legacy unit did not prove the external-DB data directory' >&2; exit 1; }
[ "$audit_unit_present" -eq 0 ] || { echo 'FAIL: the virtual legacy unit was not removed during uninstall' >&2; exit 1; }

reset_audit
DATA_DIR=/srv/legacy-writable-env
audit_env_present=1; audit_env_mode=666
audit_env_text="JWT_SECRET=synthetic-legacy-jwt-secret-000000000000"$'\n'"PORT=9090"$'\n'"DB_PATH=$DATA_DIR/meridian.db"
expect_purge_refused 'a writable legacy environment file'

reset_audit
DATA_DIR=/srv/legacy-untrusted-unit
audit_env_present=1; audit_unit_present=1; audit_unit_owner=1000
audit_env_text="JWT_SECRET=synthetic-legacy-jwt-secret-000000000000"$'\n'"PORT=9090"$'\n'"DB_PATH=/srv/separate-store/media.sqlite"
expect_purge_refused 'an untrusted unit cannot prove a foreign database path'

reset_audit
DATA_DIR=/srv/absent-meridian
audit_data_present=0
do_uninstall >/dev/null
[ "${#audit_recursive[@]}" -eq 0 ] || { echo 'FAIL: absent data directory generated a recursive removal' >&2; exit 1; }

reset_audit
DATA_DIR=/srv/foreign-install
if (validate_data_directory_for_install) >/dev/null 2>&1; then
  echo 'FAIL: a new installation adopted a nonempty foreign directory' >&2; exit 1
fi

reset_audit
DATA_DIR=/srv/empty-meridian
audit_empty_data=1
validate_data_directory_for_install
ensure_data_directory_marker
[ "$audit_marker_creations" -eq 1 ] && [[ "$audit_marker_command" = *'umask 077'* && "$audit_marker_command" = *noclobber* ]] \
  || { echo 'FAIL: a new data marker was not created exclusively with private permissions' >&2; exit 1; }
ensure_data_directory_marker
[ "$audit_marker_creations" -eq 1 ] || { echo 'FAIL: an existing data marker was replaced' >&2; exit 1; }

reset_audit
DATA_DIR=/srv/changing-meridian
audit_marker_present=1
audit_changed_after_removal=/srv/replaced-meridian
if (
  trap 'if [ "${#audit_recursive[@]}" -gt 0 ]; then exit 79; fi' EXIT
  do_uninstall >/dev/null 2>&1
); then
  echo 'FAIL: a canonical path change during uninstall was accepted' >&2; exit 1
else
  [ "$?" -ne 79 ] || { echo 'FAIL: a changed canonical path was recursively removed' >&2; exit 1; }
fi

printf 'installer purge guard tests passed (all removals captured, none executed)\n'
