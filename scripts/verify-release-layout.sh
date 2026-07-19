#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULE_PREFIX="github.com/eruca/goagents/"
APACHE_LICENSE_SHA256="cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"

published_modules=(
  "goagent|${MODULE_PREFIX}goagent|v0.1.0|goagent/v0.1.0|existing"
  "hostkit|${MODULE_PREFIX}hostkit|v0.1.0|hostkit/v0.1.0|release-delta"
  "artifactkit|${MODULE_PREFIX}artifactkit|v0.1.0|artifactkit/v0.1.0|existing"
  "contextkit|${MODULE_PREFIX}contextkit|v0.1.0|contextkit/v0.1.0|existing"
  "evalkit|${MODULE_PREFIX}evalkit|v0.1.0|evalkit/v0.1.0|existing"
  "ocrs|${MODULE_PREFIX}ocrs|v0.1.0|ocrs/v0.1.0|existing"
  "workflowkit|${MODULE_PREFIX}workflowkit|v0.1.1|workflowkit/v0.1.1|release-delta"
  "llmkit|${MODULE_PREFIX}llmkit|v0.1.0|llmkit/v0.1.0|existing"
  "mcpkit|${MODULE_PREFIX}mcpkit|v0.1.0|mcpkit/v0.1.0|existing"
  "runkit|${MODULE_PREFIX}runkit|v0.1.1|runkit/v0.1.1|release-delta"
  "skillkit|${MODULE_PREFIX}skillkit|v0.1.0|skillkit/v0.1.0|existing"
  "workflowkit/agentstep|${MODULE_PREFIX}workflowkit/agentstep|v0.1.0|workflowkit/agentstep/v0.1.0|existing"
  "mcpkit/officialsdk|${MODULE_PREFIX}mcpkit/officialsdk|v0.1.0|mcpkit/officialsdk/v0.1.0|existing"
)

release_delta_tags=(
  "hostkit/v0.1.0"
  "workflowkit/v0.1.1"
  "runkit/v0.1.1"
)

internal_requirements=(
  "llmkit|${MODULE_PREFIX}goagent|v0.1.0"
  "mcpkit|${MODULE_PREFIX}goagent|v0.1.0"
  "runkit|${MODULE_PREFIX}goagent|v0.1.0"
  "skillkit|${MODULE_PREFIX}goagent|v0.1.0"
  "workflowkit/agentstep|${MODULE_PREFIX}goagent|v0.1.0"
  "workflowkit/agentstep|${MODULE_PREFIX}workflowkit|v0.1.0"
  "mcpkit/officialsdk|${MODULE_PREFIX}goagent|v0.1.0"
  "mcpkit/officialsdk|${MODULE_PREFIX}mcpkit|v0.1.0"
  "examples/evalkit-goagent-regression|${MODULE_PREFIX}evalkit|v0.1.0"
  "examples/evalkit-goagent-regression|${MODULE_PREFIX}goagent|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}artifactkit|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}evalkit|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}goagent|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}hostkit|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}llmkit|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}runkit|v0.1.1"
  "examples/host-api|${MODULE_PREFIX}skillkit|v0.1.0"
  "examples/host-api|${MODULE_PREFIX}workflowkit|v0.1.1"
  "examples/host-runtime|${MODULE_PREFIX}artifactkit|v0.1.0"
  "examples/host-runtime|${MODULE_PREFIX}goagent|v0.1.0"
  "examples/host-runtime|${MODULE_PREFIX}llmkit|v0.1.0"
  "examples/host-runtime|${MODULE_PREFIX}runkit|v0.1.0"
  "examples/host-runtime|${MODULE_PREFIX}workflowkit|v0.1.0"
  "workflowkit/examples/agent-approval|${MODULE_PREFIX}goagent|v0.1.0"
  "workflowkit/examples/agent-approval|${MODULE_PREFIX}workflowkit|v0.1.0"
  "workflowkit/examples/agent-approval|${MODULE_PREFIX}workflowkit/agentstep|v0.1.0"
  "workflowkit/examples/ocr-review|${MODULE_PREFIX}contextkit|v0.1.0"
  "workflowkit/examples/ocr-review|${MODULE_PREFIX}goagent|v0.1.0"
  "workflowkit/examples/ocr-review|${MODULE_PREFIX}ocrs|v0.1.0"
  "workflowkit/examples/ocr-review|${MODULE_PREFIX}workflowkit|v0.1.0"
  "workflowkit/examples/ocr-review|${MODULE_PREFIX}workflowkit/agentstep|v0.1.0"
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

report_set_difference() {
  local label="$1"
  local difference="$2"

  report_error "$label differs from the release manifest"
  printf '%s\n' "$difference" >&2
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

expected_internal_requirements() {
  local owner="$1"
  local spec
  local dir
  local module
  local version

  for spec in "${internal_requirements[@]}"; do
    IFS='|' read -r dir module version <<<"$spec"
    if [[ "$dir" == "$owner" ]]; then
      printf '%s %s\n' "$module" "$version"
    fi
  done
}

actual_internal_requirements() {
  local go_mod="$1"

  awk -v prefix="$MODULE_PREFIX" '
    BEGIN { mode = "" }
    $1 == "require" && $2 == "(" { mode = "require"; next }
    $1 == "replace" && $2 == "(" { mode = "replace"; next }
    $1 == ")" { mode = ""; next }
    $1 == "require" && index($2, prefix) == 1 {
      print $2, $3
      next
    }
    mode == "require" && index($1, prefix) == 1 {
      print $1, $2
    }
  ' "$go_mod"
}

check_internal_require_versions() {
  local dir="$1"
  local difference

  if ! difference="$(diff -u \
    <(expected_internal_requirements "$dir" | LC_ALL=C sort) \
    <(actual_internal_requirements "$ROOT/$dir/go.mod" | LC_ALL=C sort))"; then
    report_set_difference "$dir internal requirements" "$difference"
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

check_workspace_replace() {
  local dir="$1"
  local module="$2"
  local version="$3"
  local target="./$dir"

  if ! awk -v module="$module" -v version="$version" -v target="$target" '
    $1 == "replace" && $2 == module && $3 == version && $4 == "=>" && $5 == target { found = 1 }
    END { exit(found ? 0 : 1) }
  ' "$ROOT/go.work"; then
    report_error "go.work: missing exact replacement $module $version => $target"
  fi
}

expected_module_dirs() {
  local spec
  local dir
  local module
  local version
  local tag
  local release_status

  for spec in "${published_modules[@]}"; do
    IFS='|' read -r dir module version tag release_status <<<"$spec"
    printf '%s\n' "$dir"
  done
  for spec in "${example_modules[@]}"; do
    IFS='|' read -r dir _ <<<"$spec"
    printf '%s\n' "$dir"
  done
}

actual_module_dirs() {
  local go_mod
  local relative

  while IFS= read -r go_mod; do
    relative="${go_mod#"$ROOT/"}"
    printf '%s\n' "${relative%/go.mod}"
  done < <(
    find "$ROOT" \
      \( -path "$ROOT/.git" -o -path "$ROOT/worktrees" -o -path '*/vendor' \) -prune -o \
      -type f -name go.mod -print
  )
}

workspace_use_dirs() {
  awk '
    $1 == "use" && $2 == "(" { in_use = 1; next }
    in_use && $1 == ")" { in_use = 0; next }
    $1 == "use" { print $2; next }
    in_use { print $1 }
  ' "$ROOT/go.work" | sed 's#^\./##; s#/$##'
}

expected_workspace_replaces() {
  local spec
  local dir
  local module
  local version
  local tag
  local release_status

  for spec in "${published_modules[@]}"; do
    IFS='|' read -r dir module version tag release_status <<<"$spec"
    printf '%s %s => ./%s\n' "$module" "$version" "$dir"
  done
}

workspace_replaces() {
  awk '
    $1 == "replace" && $2 == "(" { in_replace = 1; next }
    in_replace && $1 == ")" { in_replace = 0; next }
    $1 == "replace" {
      line = $0
      sub(/^[[:space:]]*replace[[:space:]]+/, "", line)
      print line
      next
    }
    in_replace { print $0 }
  ' "$ROOT/go.work" | awk '{$1=$1; print}'
}

check_release_manifest_sets() {
  local difference

  if ! difference="$(diff -u \
    <(expected_module_dirs | LC_ALL=C sort -u) \
    <(actual_module_dirs | LC_ALL=C sort -u))"; then
    report_set_difference "repository go.mod set" "$difference"
  fi

  if ! difference="$(diff -u \
    <(expected_module_dirs | LC_ALL=C sort -u) \
    <(workspace_use_dirs | LC_ALL=C sort -u))"; then
    report_set_difference "go.work use set" "$difference"
  fi

  if ! difference="$(diff -u \
    <(expected_workspace_replaces | LC_ALL=C sort -u) \
    <(workspace_replaces | LC_ALL=C sort -u))"; then
    report_set_difference "go.work replace set" "$difference"
  fi
}

check_release_delta_tags() {
  local spec
  local dir
  local module
  local version
  local tag
  local release_status
  local difference
  local -a manifest_delta_tags=()

  for spec in "${published_modules[@]}"; do
    IFS='|' read -r dir module version tag release_status <<<"$spec"
    case "$release_status" in
      existing)
        ;;
      release-delta)
        manifest_delta_tags+=("$tag")
        ;;
      *)
        report_error "$dir: unknown release status: $release_status"
        ;;
    esac
  done

  if ! difference="$(diff -u \
    <(printf '%s\n' "${release_delta_tags[@]}" | LC_ALL=C sort -u) \
    <(printf '%s\n' "${manifest_delta_tags[@]}" | LC_ALL=C sort -u))"; then
    report_error "release delta set mismatch"
    printf '%s\n' "$difference" >&2
  fi
}

check_example_replaces() {
  local dir="$1"
  local go_mod="$ROOT/$dir/go.mod"
  local dependency
  local target
  local target_dir
  local target_module

  while IFS= read -r dependency; do
    [[ -z "$dependency" ]] && continue
    target="$(awk -v dependency="$dependency" '
      BEGIN { mode = "" }
      $1 == "replace" && $2 == "(" { mode = "replace"; next }
      $1 == ")" { mode = ""; next }
      $1 == "replace" && $2 == dependency && $3 == "=>" { print $4; exit }
      mode == "replace" && $1 == dependency && $2 == "=>" { print $3; exit }
    ' "$go_mod")"
    if [[ -z "$target" ]]; then
      report_error "$dir: example require $dependency has no local replace"
      continue
    fi
    if [[ "$target" != ./* && "$target" != ../* ]]; then
      report_error "$dir: example replace for $dependency is not relative: $target"
      continue
    fi
    if ! target_dir="$(cd "$ROOT/$dir" && cd "$target" && pwd -P)"; then
      report_error "$dir: example replace target for $dependency does not exist: $target"
      continue
    fi
    if [[ "$target_dir" != "$ROOT"/* ]]; then
      report_error "$dir: example replace for $dependency escapes repository root: $target"
      continue
    fi
    if [[ ! -f "$target_dir/go.mod" ]]; then
      report_error "$dir: example replace target for $dependency has no go.mod: $target"
      continue
    fi
    target_module="$(module_path "$target_dir/go.mod")"
    if [[ "$target_module" != "$dependency" ]]; then
      report_error "$dir: replace $dependency points to module $target_module"
    fi
  done < <(awk -v prefix="$MODULE_PREFIX" '
    BEGIN { mode = "" }
    $1 == "require" && $2 == "(" { mode = "require"; next }
    $1 == "replace" && $2 == "(" { mode = "replace"; next }
    $1 == ")" { mode = ""; next }
    $1 == "require" && index($2, prefix) == 1 { print $2; next }
    mode == "require" && index($1, prefix) == 1 { print $1 }
  ' "$go_mod")
}

check_release_manifest_sets
check_release_delta_tags

for spec in "${published_modules[@]}"; do
  IFS='|' read -r dir module version tag release_status <<<"$spec"
  if [[ "$tag" != "$dir/$version" ]]; then
    report_error "$dir: tag is $tag, want $dir/$version"
  fi
  check_module_path "$dir" "$module"
  check_internal_require_versions "$dir"
  check_no_internal_replace "$dir"
  check_workspace_replace "$dir" "$module" "$version"
  printf 'release module: %-28s tag=%s\n' "$module" "$tag"
done

for spec in "${example_modules[@]}"; do
  IFS='|' read -r dir module <<<"$spec"
  check_module_path "$dir" "$module"
  check_internal_require_versions "$dir"
  check_example_replaces "$dir"
done

old_path_pattern='github\.com/eruca/(artifactkit|contextkit|evalkit|goagent|llmkit|mcpkit|ocrs|runkit|skillkit|workflowkit)(/|[[:space:]"`])'
if git -C "$ROOT" grep -nE "$old_path_pattern" -- '*.go' 'go.mod' 'go.work' >/dev/null; then
  report_error "Go source, go.mod, or go.work still contains pre-monorepo module paths"
fi

if [[ ! -f "$ROOT/.github/workflows/ci.yml" ]]; then
  report_error "missing repository-root GitHub Actions workflow"
fi
if [[ -f "$ROOT/goagent/.github/workflows/ci.yml" ]]; then
  report_error "nested goagent GitHub Actions workflow is not active in the monorepo"
fi

if [[ ! -f "$ROOT/LICENSE" ]]; then
  report_error "missing repository-root LICENSE"
elif ! grep -Eq '^[[:space:]]*Apache License[[:space:]]*$' "$ROOT/LICENSE" ||
  ! grep -Eq '^[[:space:]]*Version 2\.0, January 2004[[:space:]]*$' "$ROOT/LICENSE"; then
  report_error "LICENSE is not the Apache License 2.0 text"
elif ! command -v shasum >/dev/null 2>&1; then
  report_error "shasum is required to verify LICENSE"
elif [[ "$(shasum -a 256 "$ROOT/LICENSE" | awk '{print $1}')" != "$APACHE_LICENSE_SHA256" ]]; then
  report_error "LICENSE SHA-256 does not match the pinned Apache License 2.0 text"
fi

if (( failed != 0 )); then
  exit 1
fi

printf 'goagents release layout verification passed\n'
