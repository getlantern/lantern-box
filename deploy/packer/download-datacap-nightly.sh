#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <output-directory>" >&2
  exit 2
fi

require_command() {
  local command="$1"
  local install_hint="$2"
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command '$command'; $install_hint" >&2
    exit 1
  fi
}

require_command gh "install GitHub CLI: https://cli.github.com/"
require_command dpkg-deb "install the dpkg package (for example: sudo apt-get install dpkg)"

readonly output_directory="$1"
readonly repository="getlantern/lantern-box"
package_directory="$(mktemp -d)"
readonly package_directory
trap 'rm -rf "$package_directory"' EXIT

tag="$(gh release list --repo "$repository" --limit 1000 --json tagName,isDraft \
  --jq 'map(select(.isDraft == false and (.tagName | startswith("datacap-nightly-"))))[0].tagName // ""')"
if [[ -z "$tag" ]]; then
  echo "no published datacap-nightly-* release found in $repository" >&2
  exit 1
fi

mkdir -p "$output_directory"
for arch in amd64 arm64; do
  asset="datacap_${tag#datacap-nightly-}_linux_${arch}.deb"
  extract_directory="$package_directory/$arch"
  mkdir -p "$extract_directory"
  gh release download "$tag" --repo "$repository" --pattern "$asset" --dir "$package_directory"
  dpkg-deb -x "$package_directory/$asset" "$extract_directory"
  install -m 0755 "$extract_directory/usr/local/bin/datacap" "$output_directory/datacap-$arch"
done

echo "downloaded datacap $tag for amd64 and arm64"
