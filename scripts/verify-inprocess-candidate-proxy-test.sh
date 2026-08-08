#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -P "$script_dir/.." && pwd -P)"
wrapper="$script_dir/with-inprocess-candidate-proxy.sh"
source "$script_dir/lib/inprocess-candidate-proxy.sh"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-inprocess-candidate-proxy-test.XXXXXX")"

cleanup() {
  local command_status=$?

  chmod -R u+w "$workdir" || true
  rm -rf "$workdir" || true
  return "$command_status"
}
trap cleanup EXIT

workdir="$(cd -P "$workdir" && pwd -P)"
if [[ "$workdir" == "$repo_root" || "$workdir" == "$repo_root/"* ]]; then
  printf 'candidate proxy consumer must be outside the repository\n' >&2
  exit 1
fi
if [[ ! -x "$wrapper" ]]; then
  printf 'candidate proxy wrapper is missing or not executable\n' >&2
  exit 1
fi

printf 'not the project license\n' >"$workdir/wrong-license"
ln -s "$repo_root/LICENSE" "$workdir/license-link"
if package_inprocess_module "$repo_root/goagent" github.com/eruca/goagents/goagent v0.1.1 \
  "$workdir/relative-proxy" "$workdir/relative-archive" LICENSE; then
  printf 'candidate packager accepted a relative root LICENSE path\n' >&2
  exit 1
fi
if package_inprocess_module "$repo_root/goagent" github.com/eruca/goagents/goagent v0.1.1 \
  "$workdir/symlink-proxy" "$workdir/symlink-archive" "$workdir/license-link"; then
  printf 'candidate packager accepted a symlink root LICENSE\n' >&2
  exit 1
fi
if package_inprocess_module "$repo_root/goagent" github.com/eruca/goagents/goagent v0.1.1 \
  "$workdir/wrong-proxy" "$workdir/wrong-archive" "$workdir/wrong-license"; then
  printf 'candidate packager accepted an invalid root LICENSE hash\n' >&2
  exit 1
fi

consumer_root="$workdir/consumer"
mkdir -p "$consumer_root"
poison_modcache="$workdir/poison-modcache"
poison_buildcache="$workdir/poison-buildcache"
export GOMODCACHE="$poison_modcache"
export GOCACHE="$poison_buildcache"

cache_paths="$("$wrapper" bash -c 'printf "%s\\n%s\\n" "$GOMODCACHE" "$GOCACHE"')"
observed_modcache="$(sed -n '1p' <<<"$cache_paths")"
observed_buildcache="$(sed -n '2p' <<<"$cache_paths")"
if [[ "$observed_modcache" == "$poison_modcache" || "$observed_buildcache" == "$poison_buildcache" ]]; then
  printf 'candidate proxy wrapper must override caller cache paths\n' >&2
  exit 1
fi
for cache_path in "$observed_modcache" "$observed_buildcache"; do
  if [[ "$cache_path" == "$repo_root" || "$cache_path" == "$repo_root/"* ]]; then
    printf 'candidate proxy cache must be outside the repository\n' >&2
    exit 1
  fi
done

"$wrapper" bash -c '
  set -euo pipefail
  cd "$1"
  GOWORK=off go mod init example.invalid/inprocess-proxy-test >/dev/null
  GOWORK=off go mod edit \
    -require=github.com/eruca/goagents/goagent@v0.1.1 \
    -require=github.com/eruca/goagents/llmkit@v0.1.1
  cat > consumer.go <<'"'"'EOF'"'"'
package consumer

import (
  _ "github.com/eruca/goagents/goagent/ports"
  _ "github.com/eruca/goagents/llmkit/llmkit"
)
EOF
  GOWORK=off go mod tidy
  modules="$(GOWORK=off go list -m -f "{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}" all)"
  printf "%s\\n" "$modules"
  grep -Fx "github.com/eruca/goagents/goagent|v0.1.1|" <<<"$modules"
  grep -Fx "github.com/eruca/goagents/llmkit|v0.1.1|" <<<"$modules"
  expected_license_sha="cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"
  module_cache="$(GOWORK=off go env GOMODCACHE)"
  for module_name in goagent llmkit; do
    license_path="$module_cache/github.com/eruca/goagents/$module_name@v0.1.1/LICENSE"
    if [[ ! -f "$license_path" || -L "$license_path" ]]; then
      printf "candidate module %s must contain a regular non-symlink LICENSE\\n" "$module_name" >&2
      exit 1
    fi
    if command -v sha256sum >/dev/null 2>&1; then
      observed_license_sha="$(sha256sum "$license_path")"
    else
      observed_license_sha="$(shasum -a 256 "$license_path")"
    fi
    observed_license_sha="${observed_license_sha%% *}"
    if [[ "$observed_license_sha" != "$expected_license_sha" ]]; then
      printf "candidate module %s LICENSE SHA-256 is invalid\\n" "$module_name" >&2
      exit 1
    fi
  done
  if grep -E "github.com/eruca/goagents/(goagent|llmkit)\\|[^|]*\\|.+" <<<"$modules" >/dev/null; then
    printf "candidate modules must not have replacements\\n" >&2
    exit 1
  fi
  if GOPROXY="${GOPROXY%%,*}" GOWORK=off go mod download github.com/eruca/goagents/goagent@v0.1.2; then
    printf "candidate proxy forged an unavailable version\\n" >&2
    exit 1
  fi
' bash "$consumer_root"
