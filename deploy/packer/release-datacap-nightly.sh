#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <source-commit-sha> <asset-directory>" >&2
  exit 2
fi

readonly source_sha="$1"
readonly asset_directory="$2"
readonly tag="datacap-nightly-${source_sha}"

if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "invalid source commit SHA: $source_sha" >&2
  exit 2
fi

if gh release view "$tag" >/dev/null 2>&1; then
  echo "release $tag already exists"
  exit 0
fi

gh release create "$tag" "$asset_directory"/*.deb \
  --title "Datacap nightly ${source_sha}" \
  --notes "Immutable datacap VPS build from https://github.com/getlantern/lantern-cloud/commit/${source_sha}." \
  --prerelease
