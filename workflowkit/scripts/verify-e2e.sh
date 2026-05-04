#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SQLITE_DB="${TMPDIR:-/tmp}/workflowkit-e2e-verify.db"

run() {
  printf '\n==> %s\n' "$*"
  "$@"
}

run_in() {
  local dir="$1"
  shift
  printf '\n==> (cd %s && %s)\n' "$dir" "$*"
  (cd "$dir" && "$@")
}

assert_main_module_boundary() {
  printf '\n==> checking main module dependency boundary\n'
  local modules
  modules="$(cd "$ROOT" && GOWORK=off go list -m all)"
  if grep -qE '^github.com/eruca/goagent([[:space:]]|$)' <<<"$modules" ||
    grep -qE '^github.com/eruca/workflowkit/agentstep([[:space:]]|$)' <<<"$modules"; then
    printf 'main workflowkit module must not depend on goagent or agentstep\n' >&2
    printf '%s\n' "$modules" >&2
    return 1
  fi
}

rm -f "$SQLITE_DB"

run_in "$ROOT" go test ./...
run_in "$ROOT" go test -race ./...
run_in "$ROOT" go run ./examples/basic
run_in "$ROOT" go run ./examples/sqlite-resume "$SQLITE_DB"
assert_main_module_boundary

run_in "$ROOT/agentstep" go test ./...
run_in "$ROOT/examples/agent-approval" go test ./...
run_in "$ROOT/examples/agent-approval" go run .
run_in "$ROOT/examples/ocr-review" go test ./...
run_in "$ROOT/examples/ocr-review" go run .

printf '\nworkflowkit e2e verification passed\n'
