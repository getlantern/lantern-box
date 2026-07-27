#!/usr/bin/env bash
#
# Publish the Gcore lantern-box image: import a hosted qcow2 into each target
# Gcore region and poll each import task to completion. Gcore has no Packer
# plugin and no cross-region image copy, so the same URL is imported into every
# region individually. See deploy/packer/README.md.
#
# Required env:
#   GCORE_API_KEY     Gcore Cloud API token (sent as "Authorization: APIKey ...")
#   GCORE_PROJECT_ID  Gcore project ID (numeric); scopes every API call
#   VERSION           Image version label; image is named "lantern-box-$VERSION"
#   IMAGE_URL         URL Gcore's importer fetches the qcow2 from (a signed URL)
#   GCORE_REGIONS     Comma-separated numeric region IDs (e.g. "180" or "180,68")
#
# Optional env (mainly for tests):
#   CURL (default "curl"), SLEEP (default "sleep"),
#   GCORE_API_BASE (default "https://api.gcore.com/cloud/v1"),
#   POLL_ATTEMPTS (default 120), POLL_INTERVAL_SECS (default 15).
set -euo pipefail

: "${GCORE_API_KEY:?GCORE_API_KEY must be set}"
: "${GCORE_PROJECT_ID:?GCORE_PROJECT_ID must be set}"
: "${VERSION:?VERSION must be set}"
: "${IMAGE_URL:?IMAGE_URL must be set}"
: "${GCORE_REGIONS:?GCORE_REGIONS must be set (comma-separated region IDs)}"

CURL="${CURL:-curl}"
SLEEP="${SLEEP:-sleep}"
API_BASE="${GCORE_API_BASE:-https://api.gcore.com/cloud/v1}"
POLL_ATTEMPTS="${POLL_ATTEMPTS:-120}"
POLL_INTERVAL_SECS="${POLL_INTERVAL_SECS:-15}"
AUTH="Authorization: APIKey ${GCORE_API_KEY}"
IMAGE_NAME="lantern-box-${VERSION}"

# import_region REGION -> prints the created task ID on stdout.
import_region() {
  local region="$1" body resp http task
  body=$(jq -n \
    --arg name "$IMAGE_NAME" \
    --arg url "$IMAGE_URL" \
    '{name: $name, url: $url, architecture: "x86_64", os_type: "linux", os_distro: "ubuntu", os_version: "24.04"}')
  # Capture the response body AND HTTP status (NOT -f, which discards the error
  # body) so a gcore rejection is surfaced instead of an empty message.
  resp=$("$CURL" -sS -w $'\n%{http_code}' -X POST \
    -H "$AUTH" -H "Content-Type: application/json" \
    -d "$body" \
    "${API_BASE}/downloadimage/${GCORE_PROJECT_ID}/${region}")
  http="${resp##*$'\n'}"
  resp="${resp%$'\n'*}"
  if [ "$http" -lt 200 ] || [ "$http" -ge 300 ]; then
    echo "::error::gcore import in region ${region} failed: HTTP ${http}: ${resp}" >&2
    return 1
  fi
  task=$(printf '%s' "$resp" | jq -r '.tasks[0] // empty')
  if [ -z "$task" ]; then
    echo "::error::gcore import in region ${region} returned no task id (HTTP ${http}): ${resp}" >&2
    return 1
  fi
  printf '%s' "$task"
}

# poll_task REGION TASK_ID -> 0 on FINISHED, 1 on ERROR/timeout.
poll_task() {
  local region="$1" task="$2" attempt=0 state
  while [ "$attempt" -lt "$POLL_ATTEMPTS" ]; do
    state=$("$CURL" -sS -f -H "$AUTH" "${API_BASE}/tasks/${task}" | jq -r '.state')
    case "$state" in
      FINISHED)
        echo "region ${region}: import task ${task} FINISHED"
        return 0 ;;
      ERROR)
        echo "::error::region ${region}: import task ${task} state ERROR" >&2
        return 1 ;;
      *)
        echo "region ${region}: task ${task} state=${state} (attempt $((attempt + 1))/${POLL_ATTEMPTS})"
        "$SLEEP" "$POLL_INTERVAL_SECS" ;;
    esac
    attempt=$((attempt + 1))
  done
  echo "::error::region ${region}: import task ${task} did not finish after ${POLL_ATTEMPTS} attempts" >&2
  return 1
}

main() {
  echo "Publishing ${IMAGE_NAME} to Gcore regions: ${GCORE_REGIONS}"
  local failures=0 region task
  IFS=',' read -ra regions <<< "$GCORE_REGIONS"
  for region in "${regions[@]}"; do
    region="${region//[[:space:]]/}"
    [ -z "$region" ] && continue
    echo "==> Importing ${IMAGE_NAME} into region ${region}"
    if ! task=$(import_region "$region"); then
      failures=$((failures + 1))
      continue
    fi
    if ! poll_task "$region" "$task"; then
      failures=$((failures + 1))
    fi
  done
  if [ "$failures" -gt 0 ]; then
    echo "::error::${failures} gcore region import(s) failed" >&2
    exit 1
  fi
  echo "All gcore region imports finished."
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main
fi
