#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Retry only a generated-module tidy interrupted by a transient download.
# Keep all diagnostics and Go checksum verification; unknown errors fail closed.

gomobile_tidy_transport_failure() {
  awk '
    /^gomobile: go mod tidy failed: exit status [1-9][0-9]*$/ { tidy++; next }
    /^[[:space:]]*$/ { next }
    /^go: downloading [^[:space:]]+ [^[:space:]]+$/ { next }
    /^go: finding module for package [^[:space:]]+$/ { next }
    /^(go: |[[:space:]]+)[^[:space:]]+ (requires|imports)$/ { next }
    /^(go: |[[:space:]]+).*: (Get|Head) "https:\/\/[^"[:space:]]+": (.*: )?(no route to host|network is unreachable|connection reset by peer|connection refused|i\/o timeout|TLS handshake timeout|unexpected EOF|EOF)$/ { transient++; next }
    /^(go: |[[:space:]]+).*: reading https:\/\/[^[:space:]]+: (429 Too Many Requests|502 Bad Gateway|503 Service Unavailable|504 Gateway Timeout)$/ { transient++; next }
    { unknown++ }
    END { exit !(tidy == 1 && transient > 0 && unknown == 0) }
  ' "$1"
}

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi

set -euo pipefail
umask 077
[ "${1:-}" = gomobile ] && [ "${2:-}" = bind ] || {
  echo "usage: gomobile-bind-retry.sh gomobile bind [arguments...]" >&2
  exit 64
}
attempt_log="$(mktemp "${TMPDIR:-/tmp}/urnetwork-gomobile.XXXXXX")"
trap 'rm -f "$attempt_log"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for attempt in 1 2 3; do
  if "$@" >"$attempt_log" 2>&1; then
    cat "$attempt_log"
    exit 0
  else
    status=$?
  fi
  cat "$attempt_log"
  if [ "$status" -ge 128 ] || [ "$attempt" -eq 3 ] ||
      ! gomobile_tidy_transport_failure "$attempt_log"; then
    exit "$status"
  fi
  printf 'gomobile: transient dependency transport error; retrying bind (%s/3) with checksum verification unchanged\n' "$((attempt + 1))" >&2
  sleep "$attempt"
done
