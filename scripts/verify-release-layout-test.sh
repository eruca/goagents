#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source_script="$SCRIPT_DIR/verify-release-layout.sh"
test_script=""
rewrite_script=""
cleanup_error_reported=0

cleanup() {
  local cleanup_status=0
  if [[ -n "$test_script" ]]; then
    rm -f -- "$test_script" >/dev/null 2>&1 || cleanup_status=1
  fi
  if [[ -n "$rewrite_script" ]]; then
    rm -f -- "$rewrite_script" >/dev/null 2>&1 || cleanup_status=1
  fi
  return "$cleanup_status"
}
cleanup_on_exit() {
  local exit_status=$?

  trap - EXIT
  if ! cleanup; then
    if (( cleanup_error_reported == 0 )); then
      printf 'release layout test error: cleanup failed\n' >&2
    fi
    if (( exit_status == 0 )); then
      exit_status=1
    fi
  fi
  exit "$exit_status"
}
trap cleanup_on_exit EXIT

if ! bash "$source_script" >/dev/null 2>&1; then
  printf 'release layout test error: baseline verification failed\n' >&2
  exit 1
fi

if ! test_script="$(
  mktemp "$SCRIPT_DIR/.verify-release-layout-test.XXXXXX.sh" 2>/dev/null
)" || [[ -z "$test_script" ]]; then
  printf 'release layout test error: test script allocation failed\n' >&2
  exit 1
fi
if ! rewrite_script="$(
  mktemp "$SCRIPT_DIR/.verify-release-layout-rewrite.XXXXXX.sh" 2>/dev/null
)" || [[ -z "$rewrite_script" ]]; then
  printf 'release layout test error: rewrite allocation failed\n' >&2
  exit 1
fi
if ! cp "$source_script" "$test_script" >/dev/null 2>&1; then
  printf 'release layout test error: source copy failed\n' >&2
  exit 1
fi
if ! awk '
  BEGIN { changed = 0 }
  !changed && /\|existing"$/ {
    sub(/\|existing"$/, "|release-delta\"")
    changed = 1
  }
  { print }
  END { exit(changed ? 0 : 1) }
' "$test_script" >"$rewrite_script" 2>/dev/null; then
  printf 'release layout test error: existing manifest entry not found\n' >&2
  exit 1
fi
if ! mv "$rewrite_script" "$test_script" >/dev/null 2>&1; then
  printf 'release layout test error: rewrite install failed\n' >&2
  exit 1
fi

if output="$(bash "$test_script" 2>&1)"; then
  printf 'release layout test error: fourth release delta was accepted\n' >&2
  exit 1
fi
if [[ "$output" != *"release layout error: release delta set mismatch"* ]]; then
  printf 'release layout test error: stable delta mismatch was not reported\n' >&2
  exit 1
fi

if ! cleanup; then
  cleanup_error_reported=1
  printf 'release layout test error: cleanup failed\n' >&2
  exit 1
fi
trap - EXIT
printf 'release layout negative verification passed\n'
