#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

run() {
  printf '\n==> %s\n' "$*"
  "$@"
}

check_layout() {
  printf '\n==> checking single-module layout\n'
  local nested
  nested="$(find . -path ./worktrees -prune -o -path '*/testdata' -prune -o -name go.mod -print | grep -v '^\./go\.mod$' || true)"
  if [[ -n "$nested" ]]; then
    printf 'nested go.mod files are not allowed:\n%s\n' "$nested" >&2
    return 1
  fi
  if [[ -e go.work ]]; then
    printf 'go.work is not allowed; the repository is one module\n' >&2
    return 1
  fi
  if [[ "$(go list -m)" != "github.com/eruca/goagents" ]]; then
    printf 'unexpected module path: %s\n' "$(go list -m)" >&2
    return 1
  fi
  if grep -qE '^[[:space:]]*replace[[:space:](]' go.mod; then
    printf 'go.mod must not contain replace directives\n' >&2
    return 1
  fi
}

check_layout
run go mod verify
printf '\n==> checking gofmt\n'
unformatted="$(find . -path ./worktrees -prune -o -name '*.go' -print | xargs gofmt -l)"
if [[ -n "$unformatted" ]]; then
  printf 'gofmt needed:\n%s\n' "$unformatted" >&2
  exit 1
fi
run go vet ./...
run go test -race ./...

# Runnable examples double as smoke tests.
run go run ./goagent/examples/basic
run go run ./goagent/examples/artifacts
run go run ./goagent/examples/context-projection
run go run ./goagent/examples/stream
run go run ./goagent/examples/stream-sse --once
run go run ./goagent/examples/audit-log --once
run go run ./goagent/examples/approval
run go run ./goagent/examples/approval-deny
run go run ./goagent/examples/approval-resume
run go run ./goagent/examples/structured-output
run go run ./goagent/examples/openai-compatible
run go run ./mcpkit/officialsdk/examples/stdio-smoke
run go run ./mcpkit/officialsdk/examples/http-smoke
run go run ./mcpkit/officialsdk/examples/goagent-mcp-http
run go run ./examples/evalkit-goagent-regression
run go run ./examples/host-runtime
run go run ./workflowkit/examples/basic
sqlite_db="$(mktemp -d "${TMPDIR:-/tmp}/workflowkit-e2e.XXXXXX")/workflow.db"
run go run ./workflowkit/examples/sqlite-resume "$sqlite_db"
rm -rf "$(dirname "$sqlite_db")"
run go run ./workflowkit/examples/agent-approval
run go run ./workflowkit/examples/ocr-review

printf '\ngoagents verification passed\n'
