#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -P "$script_dir/.." && pwd -P)"
source "$script_dir/lib/inprocess-candidate-proxy.sh"

if (( $# == 0 )); then
  printf 'usage: %s command [arguments...]\n' "$0" >&2
  exit 2
fi

workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-inprocess-candidate-proxy.XXXXXX")"
cleanup() {
  local command_status=$?

  chmod -R u+w "$workdir" || true
  rm -rf "$workdir" || true
  return "$command_status"
}
trap cleanup EXIT

workdir="$(cd -P "$workdir" && pwd -P)"
if [[ "$workdir" == "$repo_root" || "$workdir" == "$repo_root/"* ]]; then
  printf 'candidate proxy workdir must be outside the repository\n' >&2
  exit 1
fi

proxy_root="$workdir/proxy"
archive_root="$workdir/archive"
package_inprocess_module "$repo_root/goagent" github.com/eruca/goagents/goagent v0.1.1 \
  "$proxy_root" "$archive_root" "$repo_root/LICENSE"
package_inprocess_module "$repo_root/llmkit" github.com/eruca/goagents/llmkit v0.1.1 \
  "$proxy_root" "$archive_root" "$repo_root/LICENSE"

mkdir -p "$workdir/modcache" "$workdir/buildcache"
export GOPROXY="file://$proxy_root,https://proxy.golang.org,direct"
export GONOSUMDB=github.com/eruca/goagents/goagent,github.com/eruca/goagents/llmkit
export GOMODCACHE="$workdir/modcache"
export GOCACHE="$workdir/buildcache"

if (exec "$@"); then
  exit 0
else
  exit $?
fi
