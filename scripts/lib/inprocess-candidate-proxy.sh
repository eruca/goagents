#!/usr/bin/env bash

inprocess_sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    printf 'candidate packager requires sha256sum or shasum\n' >&2
    return 1
  fi
}

package_inprocess_module() {
  local source_dir="$1"
  local module_path="$2"
  local module_version="$3"
  local proxy_root="$4"
  local archive_root="$5"
  local root_license="${6:-}"
  local version_root="$proxy_root/$module_path/@v"
  local archive_module="$archive_root/$module_path@$module_version"
  local expected_license_sha="cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"
  local observed_license_sha=""

  if [[ "$root_license" != /* ]]; then
    printf 'candidate packager root LICENSE path must be absolute\n' >&2
    return 1
  fi
  if [[ ! -f "$root_license" || -L "$root_license" ]]; then
    printf 'candidate packager root LICENSE must be a regular non-symlink file\n' >&2
    return 1
  fi
  if ! observed_license_sha="$(inprocess_sha256_file "$root_license")"; then
    return 1
  fi
  if [[ "$observed_license_sha" != "$expected_license_sha" ]]; then
    printf 'candidate packager root LICENSE SHA-256 is invalid\n' >&2
    return 1
  fi

  mkdir -p "$version_root" "$archive_module"
  cp -R "$source_dir/." "$archive_module/"
  if [[ -L "$archive_module/LICENSE" || ( -e "$archive_module/LICENSE" && ! -f "$archive_module/LICENSE" ) ]]; then
    printf 'candidate module source contains an unsafe LICENSE entry\n' >&2
    return 1
  fi
  cp "$root_license" "$archive_module/LICENSE"
  cp "$source_dir/go.mod" "$version_root/$module_version.mod"
  printf '%s\n' "$module_version" >"$version_root/list"
  printf '{"Version":"%s","Time":"2026-08-08T00:00:00Z"}\n' "$module_version" \
    >"$version_root/$module_version.info"
  find "$archive_module" -exec touch -t 198001010000 {} +
  (
    cd "$archive_root"
    find "$module_path@$module_version" -type f -print | LC_ALL=C sort | \
      zip -X -q "$version_root/$module_version.zip" -@
  )
}
