#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

run_in() {
  local dir="$1"
  shift
  printf '\n==> (cd %s && %s)\n' "$dir" "$*"
  (cd "$dir" && "$@")
}

run_in "$ROOT/contextkit" go test ./...
run_in "$ROOT/artifactkit" go test ./...
run_in "$ROOT/ocrs" go test ./...
run_in "$ROOT/runkit" go test ./...
run_in "$ROOT/llmkit" go test ./...
run_in "$ROOT/examples/host-api" go test ./...
run_in "$ROOT/examples/host-runtime" go test ./...
run_in "$ROOT/examples/host-runtime" go run .
run_in "$ROOT/workflowkit" ./scripts/verify-e2e.sh
run_in "$ROOT/goagent" make verify

printf '\ngoagents workspace verification passed\n'
