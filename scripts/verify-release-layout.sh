#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="v0.1.0"
MODULE_PREFIX="github.com/eruca/goagents/"

published_modules=(
  "goagent|${MODULE_PREFIX}goagent|goagent/v0.1.0"
  "artifactkit|${MODULE_PREFIX}artifactkit|artifactkit/v0.1.0"
  "contextkit|${MODULE_PREFIX}contextkit|contextkit/v0.1.0"
  "evalkit|${MODULE_PREFIX}evalkit|evalkit/v0.1.0"
  "ocrs|${MODULE_PREFIX}ocrs|ocrs/v0.1.0"
  "workflowkit|${MODULE_PREFIX}workflowkit|workflowkit/v0.1.0"
  "llmkit|${MODULE_PREFIX}llmkit|llmkit/v0.1.0"
  "mcpkit|${MODULE_PREFIX}mcpkit|mcpkit/v0.1.0"
  "runkit|${MODULE_PREFIX}runkit|runkit/v0.1.0"
  "skillkit|${MODULE_PREFIX}skillkit|skillkit/v0.1.0"
  "workflowkit/agentstep|${MODULE_PREFIX}workflowkit/agentstep|workflowkit/agentstep/v0.1.0"
  "mcpkit/officialsdk|${MODULE_PREFIX}mcpkit/officialsdk|mcpkit/officialsdk/v0.1.0"
)

example_modules=(
  "examples/evalkit-goagent-regression|${MODULE_PREFIX}examples/evalkit-goagent-regression"
  "examples/host-api|${MODULE_PREFIX}examples/host-api"
  "examples/host-runtime|${MODULE_PREFIX}examples/host-runtime"
  "workflowkit/examples/agent-approval|${MODULE_PREFIX}workflowkit/examples/agent-approval"
  "workflowkit/examples/ocr-review|${MODULE_PREFIX}workflowkit/examples/ocr-review"
)

failed=0

report_error() {
  printf 'release layout error: %s\n' "$*" >&2
  failed=1
}

module_path() {
  awk '$1 == "module" { print $2; exit }' "$1"
}

check_module_path() {
  local dir="$1"
  local want="$2"
  local go_mod="$ROOT/$dir/go.mod"
  local got

  if [[ ! -f "$go_mod" ]]; then
    report_error "$dir: missing go.mod"
    return
  fi
  got="$(module_path "$go_mod")"
  if [[ "$got" != "$want" ]]; then
    report_error "$dir: module path is $got, want $want"
  fi
}

check_internal_require_versions() {
  local dir="$1"
  local go_mod="$ROOT/$dir/go.mod"

  if ! awk -v prefix="$MODULE_PREFIX" -v version="$VERSION" -v file="$go_mod" '
    BEGIN { mode = ""; ok = 1 }
    $1 == "require" && $2 == "(" { mode = "require"; next }
    $1 == "replace" && $2 == "(" { mode = "replace"; next }
    $1 == ")" { mode = ""; next }
    $1 == "require" && index($2, prefix) == 1 {
      if ($3 != version) {
        printf "release layout error: %s: require %s uses %s, want %s\n", file, $2, $3, version > "/dev/stderr"
        ok = 0
      }
      next
    }
    mode == "require" && index($1, prefix) == 1 {
      if ($2 != version) {
        printf "release layout error: %s: require %s uses %s, want %s\n", file, $1, $2, version > "/dev/stderr"
        ok = 0
      }
    }
    END { exit(ok ? 0 : 1) }
  ' "$go_mod"; then
    failed=1
  fi
}

check_no_internal_replace() {
  local dir="$1"
  local go_mod="$ROOT/$dir/go.mod"
  local replacements

  replacements="$(awk -v prefix="$MODULE_PREFIX" '
    BEGIN { mode = "" }
    $1 == "replace" && $2 == "(" { mode = "replace"; next }
    $1 == ")" { mode = ""; next }
    $1 == "replace" && index($2, prefix) == 1 { print $2; next }
    mode == "replace" && index($1, prefix) == 1 { print $1 }
  ' "$go_mod")"
  if [[ -n "$replacements" ]]; then
    report_error "$dir: published module contains internal replace for $(tr '\n' ',' <<<"$replacements" | sed 's/,$//')"
  fi
}

for spec in "${published_modules[@]}"; do
  IFS='|' read -r dir module tag <<<"$spec"
  check_module_path "$dir" "$module"
  check_internal_require_versions "$dir"
  check_no_internal_replace "$dir"
  printf 'release module: %-28s tag=%s\n' "$module" "$tag"
done

for spec in "${example_modules[@]}"; do
  IFS='|' read -r dir module <<<"$spec"
  check_module_path "$dir" "$module"
  check_internal_require_versions "$dir"
done

old_path_pattern='github\.com/eruca/(artifactkit|contextkit|evalkit|goagent|llmkit|mcpkit|ocrs|runkit|skillkit|workflowkit)(?=/|[[:space:]"`])'
if rg -n --pcre2 "$old_path_pattern" --glob '*.go' --glob 'go.mod' "$ROOT" >/dev/null; then
  report_error "Go source or go.mod still contains pre-monorepo module paths"
fi

if (( failed != 0 )); then
  exit 1
fi

printf 'goagents release layout verification passed\n'
