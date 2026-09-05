#!/usr/bin/env bash
# Serialize tracked writers and consumers of the current working directory's
# Android SDK output. The file contains diagnostics only; live ownership is the
# kernel lock retained on descriptor 8 across child processes.

sdk_android_output_lock_error() {
  printf 'Android SDK output gate: %s\n' "$1" >&2
}

sdk_android_output_lock_try_fd8() {
  case "$(uname -s)" in
    Darwin)
      [ -x /usr/bin/lockf ] || return 69
      /usr/bin/lockf -s -t 0 8
      ;;
    Linux)
      command -v flock >/dev/null 2>&1 || return 69
      flock -n 8
      ;;
    *) return 69 ;;
  esac
}

sdk_android_output_lock_fd8_matches_path() {
  local lock_path="$1"
  case "$(uname -s)" in
    Darwin)
      # Bash 3 compares /dev/fd's devfs inode rather than the open file's
      # inode. Compare fstat(8) with stat(path) through macOS's system Perl.
      [ -x /usr/bin/perl ] || return 69
      /usr/bin/perl -e '
        open(my $lock, ">&=8") or exit 1;
        my @descriptor = stat($lock);
        my @path = stat($ARGV[0]);
        exit(!(@descriptor && @path &&
          $descriptor[0] == $path[0] && $descriptor[1] == $path[1]));
      ' "$lock_path"
      ;;
    Linux) [ "/proc/$$/fd/8" -ef "$lock_path" ] ;;
    *) return 69 ;;
  esac
}

# Metadata is not authority, but tying the inherited token to the locked inode
# catches stale or forged environment markers instead of trusting them.
sdk_android_output_lock_metadata_matches() {
  local lock_path="$1" expected_token="$2"
  local key value version='' role='' pid='' token='' started_utc=''
  local version_seen=0 role_seen=0 pid_seen=0 token_seen=0 started_seen=0

  while IFS='=' read -r key value; do
    case "$key" in
      version)
        [ "$version_seen" -eq 0 ] || return 1
        version_seen=1
        version="$value"
        ;;
      role)
        [ "$role_seen" -eq 0 ] || return 1
        role_seen=1
        role="$value"
        ;;
      pid)
        [ "$pid_seen" -eq 0 ] || return 1
        pid_seen=1
        pid="$value"
        ;;
      token)
        [ "$token_seen" -eq 0 ] || return 1
        token_seen=1
        token="$value"
        ;;
      started_utc)
        [ "$started_seen" -eq 0 ] || return 1
        started_seen=1
        started_utc="$value"
        ;;
      *) return 1 ;;
    esac
  done <"$lock_path" || return 1

  [ "$version_seen" -eq 1 ] && [ "$version" = 1 ] &&
    [ "$role_seen" -eq 1 ] &&
    case "$role" in ''|*[!A-Za-z0-9._/-]*) false ;; *) true ;; esac &&
    [ "$pid_seen" -eq 1 ] &&
    case "$pid" in ''|*[!0-9]*) false ;; *) true ;; esac &&
    [ "$token_seen" -eq 1 ] &&
    case "$token" in
      *[!0-9a-f]*|'') false ;;
      *) [ "${#token}" -eq 32 ] ;;
    esac &&
    [ "$token" = "$expected_token" ] &&
    [ "$started_seen" -eq 1 ] &&
    case "$started_utc" in ''|*[!0-9TZ:-]*) false ;; *) true ;; esac
}

sdk_android_output_lock_verify_held() {
  local output_dir lock_path lock_status
  output_dir="$(pwd -P)" || return 70
  lock_path="$output_dir/.android-output.lock"

  if [ "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_HELD:-}" != 1 ] ||
     [ "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_DIR:-}" != "$output_dir" ] ||
     [ "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_PATH:-}" != "$lock_path" ] ||
     [ -z "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_TOKEN:-}" ] ||
     [ -z "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_ROLE:-}" ] ||
     [ -L "$lock_path" ] || [ ! -f "$lock_path" ]; then
    sdk_android_output_lock_error \
      "inherited ownership does not match this output"
    return 70
  fi
  if ! { : >&8; } 2>/dev/null ||
     ! sdk_android_output_lock_fd8_matches_path "$lock_path"; then
    sdk_android_output_lock_error \
      "inherited ownership does not match this output"
    return 70
  fi

  lock_status=0
  sdk_android_output_lock_try_fd8 || lock_status=$?
  if [ "$lock_status" -ne 0 ]; then
    if [ "$lock_status" -eq 69 ]; then
      sdk_android_output_lock_error "kernel locking primitive is unavailable"
      return 69
    fi
    sdk_android_output_lock_error "inherited descriptor does not own the kernel lock"
    return 70
  fi
  if ! sdk_android_output_lock_metadata_matches \
      "$lock_path" "$URNETWORK_ANDROID_SDK_OUTPUT_LOCK_TOKEN"; then
    sdk_android_output_lock_error "inherited ownership metadata does not match"
    return 70
  fi
}

# Acquire and retain output ownership in the current shell. Callers that source
# this file keep descriptor 8 until they explicitly close it or exit.
sdk_android_output_lock_acquire() {
  local role="${1:-}" output_dir lock_path lock_status owner_role owner_pid
  local token started_utc key value

  case "$role" in
    ''|*[!A-Za-z0-9._/-]*)
      sdk_android_output_lock_error "invalid role"
      return 64
      ;;
  esac
  output_dir="$(pwd -P)" || return 70
  lock_path="$output_dir/.android-output.lock"

  if [ "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_HELD:-}" = 1 ]; then
    sdk_android_output_lock_verify_held
    return $?
  fi
  if [ -n "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_HELD:-}" ] ||
     [ -n "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_DIR:-}" ] ||
     [ -n "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_PATH:-}" ] ||
     [ -n "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_TOKEN:-}" ] ||
     [ -n "${URNETWORK_ANDROID_SDK_OUTPUT_LOCK_ROLE:-}" ]; then
    sdk_android_output_lock_error "invalid inherited ownership marker"
    return 70
  fi
  if { : >&8; } 2>/dev/null; then
    sdk_android_output_lock_error "descriptor 8 is already in use"
    return 70
  fi
  if [ -L "$lock_path" ] || { [ -e "$lock_path" ] && [ ! -f "$lock_path" ]; }; then
    sdk_android_output_lock_error "lock path is not a regular file"
    return 73
  fi
  : >>"$lock_path" || {
    sdk_android_output_lock_error "cannot open lock"
    return 73
  }
  exec 8>>"$lock_path" || {
    sdk_android_output_lock_error "cannot retain lock descriptor"
    return 73
  }
  if ! sdk_android_output_lock_fd8_matches_path "$lock_path"; then
    exec 8>&-
    sdk_android_output_lock_error "lock descriptor inode does not match lock path"
    return 73
  fi

  lock_status=0
  sdk_android_output_lock_try_fd8 || lock_status=$?
  if [ "$lock_status" -ne 0 ]; then
    if [ "$lock_status" -eq 69 ]; then
      exec 8>&-
      sdk_android_output_lock_error "kernel locking primitive is unavailable"
      return 69
    fi
    owner_role=unknown
    owner_pid=unknown
    while IFS='=' read -r key value; do
      case "$key" in
        role)
          case "$value" in ''|*[!A-Za-z0-9._/-]*) ;; *) owner_role="$value" ;; esac
          ;;
        pid)
          case "$value" in ''|*[!0-9]*) ;; *) owner_pid="$value" ;; esac
          ;;
      esac
    done <"$lock_path"
    exec 8>&-
    sdk_android_output_lock_error "busy (role=$owner_role pid=$owner_pid)"
    return 75
  fi

  token="$(LC_ALL=C od -An -N16 -tx1 /dev/urandom)" || {
    exec 8>&-
    sdk_android_output_lock_error "cannot create ownership token"
    return 70
  }
  token="${token//[[:space:]]/}"
  case "$token" in
    *[!0-9a-f]*|'')
      exec 8>&-
      sdk_android_output_lock_error "cannot create ownership token"
      return 70
      ;;
  esac
  if [ "${#token}" -ne 32 ]; then
    exec 8>&-
    sdk_android_output_lock_error "cannot create ownership token"
    return 70
  fi
  started_utc="$(date -u +%Y-%m-%dT%H:%M:%SZ)" || {
    exec 8>&-
    sdk_android_output_lock_error "cannot record ownership time"
    return 70
  }
  if ! printf 'version=1\nrole=%s\npid=%s\ntoken=%s\nstarted_utc=%s\n' \
      "$role" "$$" "$token" "$started_utc" >"$lock_path"; then
    exec 8>&-
    sdk_android_output_lock_error "cannot record ownership metadata"
    return 73
  fi
  chmod 600 "$lock_path" || {
    exec 8>&-
    sdk_android_output_lock_error "cannot protect ownership metadata"
    return 73
  }

  export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_HELD=1
  export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_DIR="$output_dir"
  export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_PATH="$lock_path"
  export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_TOKEN="$token"
  export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_ROLE="$role"
  sdk_android_output_lock_verify_held
}

sdk_android_output_lock_main() {
  local role
  if [ "$#" -eq 1 ] && [ "$1" = --verify-held ]; then
    sdk_android_output_lock_verify_held
    return $?
  fi
  if [ "$#" -lt 3 ]; then
    echo "usage: $0 ROLE -- COMMAND [ARG ...]" >&2
    return 64
  fi
  role="$1"
  shift
  if [ "$1" != -- ]; then
    echo "usage: $0 ROLE -- COMMAND [ARG ...]" >&2
    return 64
  fi
  shift
  sdk_android_output_lock_acquire "$role" || return $?
  exec "$@"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -Eeuo pipefail
  umask 077
  sdk_android_output_lock_main "$@"
fi
