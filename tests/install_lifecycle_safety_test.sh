#!/usr/bin/env bash
# shellcheck disable=SC2034,SC2317,SC2329
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck disable=SC1091
source "${REPO_ROOT}/install.sh"

# Run the real public installer operations with an entirely virtual filesystem
# and service manager. No product deletion, rename, privileged command or host
# service call can escape as_root, even when a failure path exits the shell.
init_privilege() { ROOT_PREFIX=(); }
is_systemd() { return 0; }
need_cmd() { :; }
id() { return 1; }
getent() { return 1; }
mktemp() { printf '/srv/meridian-lifecycle-test-work\n'; }
chmod() { :; }
remove_managed_nginx_config() { printf 'TRACE remove-nginx\n'; }
validate_existing_secret_configuration() { return 0; }
validate_data_directory_for_install() { validate_data_dir; }
migrate_managed_nginx_redaction() { return 0; }
prepare_data_and_config() { return 0; }
migrate_update_systemd_service() { return 0; }
restore_update_systemd_service() { return 0; }
fix_database_permissions() { return 0; }
get_current_version() { printf 'v1.11.9\n'; }
resolve_latest_version() { printf 'v1.11.10\n'; }
download_release_binary() { DOWNLOADED_BINARY=/srv/synthetic-new-binary; }
read_env_value() {
  if [ "$1" = DB_PATH ]; then printf '%s/meridian.db\n' "$DATA_DIR"; else printf '9090\n'; fi
}
service_is_active() { [ "$audit_state" = active ]; }
create_backup_archive() {
  [ "$audit_state" = inactive ] || return 1
  LAST_BACKUP_PATH=/srv/synthetic-backup.tar.gz
  printf 'TRACE backup-inactive\n'
}
wait_for_health() {
  audit_health_calls=$((audit_health_calls + 1))
  if [ "$audit_health_calls" = 1 ]; then
    printf 'TRACE health-failed state=%s\n' "$audit_state"
    return 1
  fi
  return 0
}

function [ {
  if builtin [ "$#" = 4 ] && builtin [ "$1" = '!' ] && builtin [ "$2" = -f ] && builtin [ "$3" = "$SERVICE_FILE" ]; then return 1; fi
  if builtin [ "$#" = 3 ]; then
    case "$1:$2" in
      "-f:$SERVICE_FILE"|"-f:$PREVIOUS_BIN"|"-x:$INSTALL_DIR/$BIN_NAME"|-d:/srv/meridian-lifecycle-test-work|-d:/srv/meridian-lifecycle-test-work/data-snapshot) return 0 ;;
    esac
  fi
  builtin [ "$@"
}

reset_audit() {
  DATA_DIR=/srv/meridian-lifecycle-test-data
  audit_state=active; audit_main_pid=123; audit_control_pid=0
  audit_stop_mode=normal; audit_show_mode=normal
  audit_stop_calls=0; audit_health_calls=0; audit_started_new=0
  audit_block_rollback=0; audit_stop_with_live_pid=0
  ASSUME_YES=1; PURGE_DATA=1; UNINSTALL_KEEP_DATA=0
  UPDATE_TRANSACTION=0; UPDATE_TMP_DIR=''; UPDATE_SNAPSHOT_DIR=''
  UPDATE_SNAPSHOT_RESTORED=0; UPDATE_BINARY_CHANGED=0; UPDATE_AGENT_CHANGED=0
  UPDATE_SERVICE_CHANGED=0; UPDATE_SERVICE_SNAPSHOT=''; UPDATE_WAS_ACTIVE=0
}

as_root() {
  local operation="$1" target source
  shift; target="${!#}"
  case "$operation" in
    test) case "$1" in -L) return 1 ;; -f|-d|-e) return 0 ;; *) return 99 ;; esac ;;
    bash) printf '%s\n' "$target" ;;
    stat) printf '0:600\n' ;;
    cat) printf '%s\n' "$DATA_DIRECTORY_SIGNATURE" ;;
    find) return 0 ;;
    systemctl)
      case "$1" in
        stop)
          audit_stop_calls=$((audit_stop_calls + 1))
          printf 'TRACE stop state=%s\n' "$audit_state"
          if [ "$audit_block_rollback" = 1 ] && [ "$audit_started_new" = 1 ]; then return 1; fi
          case "$audit_stop_mode" in
            refused) return 1 ;;
            false-success) return 0 ;;
            failed-dead) audit_state=failed; audit_main_pid=0; return 1 ;;
            *) audit_state=inactive; audit_main_pid=0 ;;
          esac
          if [ "$audit_stop_with_live_pid" = 1 ]; then audit_main_pid=456; fi
          ;;
        show)
          [ "$audit_show_mode" != error ] || return 1
          if [ "$audit_show_mode" = malformed ]; then printf 'ActiveState=inactive\nMainPID=unknown\nControlPID=0\n'
          elif [ "$audit_show_mode" = missing ]; then printf 'ActiveState=inactive\nMainPID=0\n'
          else printf 'ActiveState=%s\nMainPID=%s\nControlPID=%s\n' "$audit_state" "$audit_main_pid" "$audit_control_pid"
          fi
          ;;
        restart|start)
          audit_started_new=1; audit_state=active; audit_main_pid=789
          printf 'TRACE restart-active\n'
          ;;
        *) return 0 ;;
      esac
      ;;
    install|cp|chown|chmod) return 0 ;;
    mv)
      local -a arguments=("$@")
      source="${arguments[${#arguments[@]}-2]}"
      if [ "$source" = "$DATA_DIR" ] || [[ "$source" = */.meridian-lifecycle-test-data.restore.* ]]; then
        printf 'TRACE data-swap state=%s pid=%s control=%s\n' "$audit_state" "$audit_main_pid" "$audit_control_pid"
      elif [[ "$target" = "$INSTALL_DIR"/* ]]; then printf 'TRACE binary-replacement\n'
      fi
      ;;
    rm)
      if [ "$target" = /srv/meridian-lifecycle-test-work ]; then printf 'TRACE temporary-cleanup\n'
      elif [ "$target" = "$DATA_DIR" ] || [[ "$target" = */.meridian-lifecycle-test-data.pre-restore.* ]]; then
        printf 'TRACE data-delete state=%s pid=%s control=%s\n' "$audit_state" "$audit_main_pid" "$audit_control_pid"
      elif [[ "$*" = *"$INSTALL_DIR/$BIN_NAME"* ]] || [ "$target" = "$SERVICE_FILE" ]; then printf 'TRACE product-delete\n'
      fi
      ;;
    *) printf 'BLOCKED unexpected privileged command: %s\n' "$operation" >&2; return 99 ;;
  esac
}

fail_test() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_absent() { if [[ "$1" = *"$2"* ]]; then fail_test "$3"; fi; }

reset_audit
set +e
audit_output=$( (set -e; do_update) 2>&1 )
audit_status=$?
set -e
[ "$audit_status" != 0 ] || fail_test 'failed health check did not fail the update'
[[ "$audit_output" = *'TRACE backup-inactive'* ]] || fail_test 'update did not take its initial backup while inactive'
[[ "$audit_output" = *'TRACE data-swap state=inactive pid=0 control=0'* ]] || fail_test 'rollback did not confirm an inactive process before the data swap'
assert_absent "$audit_output" 'TRACE data-swap state=active' 'health failure restored data while the new process was still active'
assert_absent "$audit_output" 'TRACE data-delete state=active' 'health failure deleted live process data'
printf 'PASS update health failure stops and confirms the process before rollback\n'

reset_audit; audit_block_rollback=1
set +e
audit_output=$( (set -e; do_update) 2>&1 )
audit_status=$?
set -e
[ "$audit_status" != 0 ] || fail_test 'blocked rollback unexpectedly succeeded'
assert_absent "$audit_output" 'TRACE data-swap' 'blocked rollback changed live data'
assert_absent "$audit_output" 'TRACE data-delete' 'blocked rollback deleted live data'
assert_absent "$audit_output" 'TRACE temporary-cleanup' 'blocked rollback deleted the preserved recovery snapshot'
printf 'PASS failed stop preserves live data and the update snapshot\n'

reset_audit; audit_started_new=1; audit_block_rollback=1
set +e
audit_output=$( (
  UPDATE_TRANSACTION=1; UPDATE_BINARY_CHANGED=1
  UPDATE_TMP_DIR=/srv/meridian-lifecycle-test-work
  UPDATE_SNAPSHOT_DIR=/srv/meridian-lifecycle-test-work/data-snapshot
  false
  cleanup_update_transaction
) 2>&1 )
audit_status=$?
set -e
[ "$audit_status" != 0 ] || fail_test 'EXIT rollback lost its failure status'
assert_absent "$audit_output" 'TRACE binary-replacement' 'EXIT rollback replaced binaries before a successful stop'
assert_absent "$audit_output" 'TRACE data-swap' 'EXIT rollback changed live data after stop failed'
assert_absent "$audit_output" 'TRACE temporary-cleanup' 'EXIT rollback removed the recovery snapshot after stop failed'
printf 'PASS EXIT rollback also preserves files when stopping cannot be confirmed\n'

for audit_case in refused false-success query-error query-malformed query-missing live-main-pid live-control-pid; do
  reset_audit
  case "$audit_case" in
    refused|false-success) audit_stop_mode="$audit_case" ;;
    query-error) audit_show_mode=error ;;
    query-malformed) audit_show_mode=malformed ;;
    query-missing) audit_show_mode=missing ;;
    live-main-pid) audit_stop_with_live_pid=1 ;;
    live-control-pid) audit_control_pid=987 ;;
  esac
  set +e
  audit_output=$( (set -e; do_uninstall) 2>&1 )
  audit_status=$?
  set -e
  [ "$audit_status" != 0 ] || fail_test "uninstall accepted $audit_case"
  assert_absent "$audit_output" 'TRACE remove-nginx' "uninstall removed configuration before confirming $audit_case"
  assert_absent "$audit_output" 'TRACE product-delete' "uninstall removed product files in $audit_case"
  assert_absent "$audit_output" 'TRACE data-delete' "uninstall purged live or unconfirmed data in $audit_case"
  printf 'PASS uninstall refused %s before any removal\n' "$audit_case"
done

for audit_case in normal failed-dead; do
  reset_audit; audit_stop_mode="$audit_case"
  if ! audit_output=$( (set -e; do_uninstall) 2>&1 ); then fail_test "uninstall rejected verified $audit_case service"; fi
  [[ "$audit_output" = *'TRACE product-delete'* ]] || fail_test "verified $audit_case uninstall did not remove the product"
  [[ "$audit_output" = *'TRACE data-delete state='*'pid=0 control=0'* ]] || fail_test "verified $audit_case uninstall did not purge after zero PIDs"
  printf 'PASS uninstall accepts verified %s with no main or control process\n' "$audit_case"
done

printf 'installer lifecycle safety tests passed (all mutations captured, none executed)\n'
