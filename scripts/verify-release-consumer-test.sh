#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
consumer="$repo_root/scripts/verify-release-consumer.sh"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-release-consumer-test.XXXXXX")"
cleanup() {
  rm -rf "$workdir"
}
trap cleanup EXIT

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
if [[ "$1" == "get" || ( "$1" == "mod" && "$2" == "tidy" ) || "$1" == "test" ]]; then
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
candidate="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
