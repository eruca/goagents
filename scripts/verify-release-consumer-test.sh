#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
consumer="$repo_root/scripts/verify-release-consumer.sh"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-release-consumer-test.XXXXXX")"
repo_tmpdir=""
remove_owned_tree() {
  local owned_path="$1"
  local cleanup_status=0

  # The harness calls this only for directories it created with mktemp.
  [[ -d "$owned_path" ]] || return 0
  if ! chmod -R u+w "$owned_path"; then
    cleanup_status=1
  fi
  if ! rm -rf "$owned_path"; then
    cleanup_status=1
  fi
  return "$cleanup_status"
}
cleanup() {
  local command_status=$?
  local cleanup_status=0

  if ! remove_owned_tree "$workdir"; then
    cleanup_status=1
  fi
  if [[ -n "$repo_tmpdir" ]] && ! remove_owned_tree "$repo_tmpdir"; then
    cleanup_status=1
  fi
  if (( cleanup_status != 0 )); then
    printf 'release consumer test cleanup failed\n' >&2
    return 1
  fi
  return "$command_status"
}
trap cleanup EXIT

cleanup_probe="$workdir/cleanup-probe"
mkdir -p "$cleanup_probe/readonly"
printf 'read-only cleanup probe\n' >"$cleanup_probe/readonly/marker"
chmod 444 "$cleanup_probe/readonly/marker"
chmod 555 "$cleanup_probe/readonly"
if ! remove_owned_tree "$cleanup_probe"; then
  printf 'read-only outer cleanup probe failed\n' >&2
  exit 1
fi
if [[ -e "$cleanup_probe" ]]; then
  printf 'read-only outer cleanup probe left a residue\n' >&2
  exit 1
fi

fake_bin="$workdir/bin"
mkdir -p "$fake_bin"
cat >"$fake_bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"$FAKE_GO_CALLS"

if [[ "$1" == "mod" && "$2" == "init" ]]; then
  printf 'module example.invalid/release-consumer-test\n\ngo 1.24\n' >go.mod
  exit 0
fi
if [[ "$1" == "get" ]]; then
  if [[ "${FAKE_CREATE_READONLY_MODCACHE:-0}" == "1" ]]; then
    readonly_module="$GOMODCACHE/example.invalid/readonly@v0.0.0"
    mkdir -p "$readonly_module"
    printf 'readonly module cache entry\n' >"$readonly_module/go.mod"
    chmod 444 "$readonly_module/go.mod"
    chmod 555 "$readonly_module"
  fi
  exit 0
fi
if [[ ( "$1" == "mod" && "$2" == "tidy" ) || "$1" == "test" ]]; then
  exit 0
fi
if [[ "$1" == "list" && "$2" == "-m" && "$3" == "all" ]]; then
  printf 'example.invalid/release-consumer-test\n%s %s\n' \
    "$FAKE_MODULE_PATH" "$FAKE_RESOLVED_VERSION"
  exit 0
fi
if [[ "$1" == "list" && "$2" == "-m" && "$3" == "-f" ]]; then
  case "$4" in
    '{{if .Replace}}{{.Path}}=>{{.Replace.Path}}{{end}}')
      exit 0
      ;;
    '{{.Path}}|{{.Version}}')
      printf '%s|%s\n' "$FAKE_MODULE_PATH" "$FAKE_RESOLVED_VERSION"
      exit 0
      ;;
    '{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}')
      printf '%s|%s|\n' "$FAKE_MODULE_PATH" "$FAKE_RESOLVED_VERSION"
      exit 0
      ;;
  esac
fi

printf 'unexpected fake go invocation\n' >&2
exit 2
EOF
chmod +x "$fake_bin/go"

export PATH="$fake_bin:$PATH"
export FAKE_GO_CALLS="$workdir/go.calls"
export FAKE_MODULE_PATH="github.com/eruca/goagents/hostkit"
export FAKE_RESOLVED_VERSION="v0.0.0-20260719000000-bbbbbbbbbbbb"

failures=0
candidate="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

assert_repo_tmpdir_is_rejected() {
  local tmpdir="$1"
  local label="$2"
  local log="$workdir/$label.log"

  : >"$FAKE_GO_CALLS"
  if TMPDIR="$tmpdir" "$consumer" hostkit "$candidate" pseudo >"$log" 2>&1; then
    printf '%s repo TMPDIR unexpectedly succeeded\n' "$label" >&2
    failures=$((failures + 1))
  fi
  if [[ -s "$FAKE_GO_CALLS" ]]; then
    printf '%s repo TMPDIR reached the go command\n' "$label" >&2
    failures=$((failures + 1))
  fi
  if grep -Fq 'PASS' "$log"; then
    printf '%s repo TMPDIR emitted PASS\n' "$label" >&2
    failures=$((failures + 1))
  fi
  if grep -Fq "$repo_root" "$log" || grep -Fq "$repo_tmpdir" "$log" || \
    grep -Fq "$tmpdir" "$log" || grep -Fq "$workdir" "$log"
  then
    printf '%s repo TMPDIR exposed a local path\n' "$label" >&2
    failures=$((failures + 1))
  fi
  if [[ ! -f "$repo_tmpdir/marker" ]]; then
    printf '%s repo TMPDIR removed the caller marker\n' "$label" >&2
    failures=$((failures + 1))
  fi
  if find "$repo_tmpdir" -mindepth 1 -maxdepth 1 ! -name marker -print -quit | grep -q .; then
    printf '%s repo TMPDIR left its temporary child behind\n' "$label" >&2
    failures=$((failures + 1))
  fi
}

repo_tmpdir="$(mktemp -d "$repo_root/.goagents-release-consumer-test.XXXXXX")"
printf 'caller-marker\n' >"$repo_tmpdir/marker"
export FAKE_RESOLVED_VERSION="v0.0.0-20260719000000-aaaaaaaaaaaa"
assert_repo_tmpdir_is_rejected "$repo_tmpdir" direct

repo_tmpdir_link="$workdir/repo-tmpdir-link"
ln -s "$repo_tmpdir" "$repo_tmpdir_link"
assert_repo_tmpdir_is_rejected "$repo_tmpdir_link" symlink

consumer_tmpdir="$workdir/consumer-tmp"
mkdir -p "$consumer_tmpdir"
readonly_cleanup_log="$workdir/readonly-cleanup.log"
export FAKE_RESOLVED_VERSION="v0.0.0-20260719000000-aaaaaaaaaaaa"
if ! FAKE_CREATE_READONLY_MODCACHE=1 TMPDIR="$consumer_tmpdir" \
  "$consumer" hostkit "$candidate" pseudo >"$readonly_cleanup_log" 2>&1
then
  printf 'read-only module cache cleanup unexpectedly failed\n' >&2
  failures=$((failures + 1))
fi
if find "$consumer_tmpdir" -mindepth 1 -print -quit | grep -q .; then
  printf 'read-only module cache cleanup left its temporary child behind\n' >&2
  failures=$((failures + 1))
fi

invalid_log="$workdir/invalid.log"
: >"$FAKE_GO_CALLS"
if "$consumer" hostkit HEAD pseudo >"$invalid_log" 2>&1; then
  printf 'invalid pseudo SHA unexpectedly succeeded\n' >&2
  failures=$((failures + 1))
fi
if [[ -s "$FAKE_GO_CALLS" ]]; then
  printf 'invalid pseudo SHA reached the go command\n' >&2
  failures=$((failures + 1))
fi
if grep -Fq 'PASS' "$invalid_log"; then
  printf 'invalid pseudo SHA emitted PASS\n' >&2
  failures=$((failures + 1))
fi

mismatch_log="$workdir/mismatch.log"
: >"$FAKE_GO_CALLS"
export FAKE_RESOLVED_VERSION="v0.0.0-20260719000000-bbbbbbbbbbbb"
if "$consumer" hostkit "$candidate" pseudo >"$mismatch_log" 2>&1; then
  printf 'pseudo-version revision mismatch unexpectedly succeeded\n' >&2
  failures=$((failures + 1))
fi
if grep -Fq 'PASS' "$mismatch_log"; then
  printf 'pseudo-version revision mismatch emitted PASS\n' >&2
  failures=$((failures + 1))
fi

if (( failures != 0 )); then
  exit 1
fi
printf 'release consumer negative checks: PASS\n'
