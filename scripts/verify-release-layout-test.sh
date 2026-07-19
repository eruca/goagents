#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source_script="$SCRIPT_DIR/verify-release-layout.sh"
test_script=""
rewrite_script=""

cleanup() {
  local cleanup_status=0
  if [[ -n "$test_script" ]]; then
    rm -f -- "$test_script" || cleanup_status=1
  fi
  if [[ -n "$rewrite_script" ]]; then
    rm -f -- "$rewrite_script" || cleanup_status=1
  fi
  return "$cleanup_status"
}
trap cleanup EXIT

test_script="$(mktemp "$SCRIPT_DIR/.verify-release-layout-test.XXXXXX.sh")"
rewrite_script="$(mktemp "$SCRIPT_DIR/.verify-release-layout-rewrite.XXXXXX.sh")"
cp "$source_script" "$test_script"
if ! awk '
  BEGIN { changed = 0 }
  !changed && /^[[:space:]]*"goagent\|.*\|existing"$/ {
    sub(/\|existing"$/, "|release-delta\"")
    changed = 1
  }
  { print }
  END { exit(changed ? 0 : 1) }
' "$test_script" >"$rewrite_script"; then
  printf 'release layout test error: existing manifest entry not found\n' >&2
  exit 1
fi
mv "$rewrite_script" "$test_script"

if output="$(bash "$test_script" 2>&1)"; then
  printf 'release layout test error: fourth release delta was accepted\n' >&2
  exit 1
fi
if [[ "$output" != *"release layout error: release delta set mismatch"* ]]; then
  printf 'release layout test error: stable delta mismatch was not reported\n' >&2
  exit 1
fi

printf 'release layout negative verification passed\n'
