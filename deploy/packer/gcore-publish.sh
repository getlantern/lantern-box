#!/usr/bin/env bash
#
# Import a hosted qcow2 into each target Gcore region, poll each task to
# completion, and check the resulting image's visibility — deleting it if it landed
# in gcore's public catalog. Gcore has no Packer plugin and no cross-region image
# copy, so the same URL is imported per region. See deploy/packer/README.md.
#
# Required env:
#   GCORE_API_KEY     Gcore Cloud API token ("Authorization: APIKey ...")
#   GCORE_PROJECT_ID  Gcore project ID (numeric); scopes every API call
#   VERSION           Image is named "lantern-box-$VERSION"
#   IMAGE_URL         URL Gcore's importer fetches the qcow2 from
#   GCORE_REGIONS     Comma-separated numeric region IDs (e.g. "180,68")
#
# Optional env (mainly for tests):
#   CURL (default "curl"), SLEEP (default "sleep"),
#   GCORE_API_BASE (default "https://api.gcore.com/cloud/v1"),
#   POLL_ATTEMPTS (default 120), POLL_INTERVAL_SECS (default 15),
#   DELETE_POLL_ATTEMPTS (default 20) — deletes are quick, and this only runs on the
#   already-failing public-image path, so it must not eat the whole job timeout.
#   VISIBILITY_ATTEMPTS (default 3) — tries for each of the two visibility lookups,
#   which fail the publish once exhausted (see report_visibility).
#   IMPORT_STAGGER_SECS (default 30) — pause between import POSTs; 0 disables.
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
DELETE_POLL_ATTEMPTS="${DELETE_POLL_ATTEMPTS:-20}"
VISIBILITY_ATTEMPTS="${VISIBILITY_ATTEMPTS:-3}"
IMPORT_STAGGER_SECS="${IMPORT_STAGGER_SECS:-30}"
AUTH="Authorization: APIKey ${GCORE_API_KEY}"
IMAGE_NAME="lantern-box-${VERSION}"

# import_region REGION -> prints the created task ID on stdout.
import_region() {
  local region="$1" body resp http task
  body=$(jq -n \
    --arg name "$IMAGE_NAME" \
    --arg url "$IMAGE_URL" \
    '{name: $name, url: $url, architecture: "x86_64", os_type: "linux", os_distro: "ubuntu", os_version: "24.04"}')
  # Capture body AND status (not -f, which discards the error body) so a gcore
  # rejection is surfaced instead of an empty message.
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

# poll_task REGION TASK_ID [WHAT] [MAX_ATTEMPTS] -> 0 on FINISHED, 1 on ERROR/timeout.
# MAX_ATTEMPTS is a parameter, not an env override, so a caller wanting a shorter budget
# cannot shadow POLL_ATTEMPTS for everyone. In practice this is the delete path's poller;
# main() waits on the imports together.
poll_task() {
  local region="$1" task="$2" what="${3:-import}" max="${4:-$POLL_ATTEMPTS}" attempt=0 state
  while [ "$attempt" -lt "$max" ]; do
    state=$("$CURL" -sS -f -H "$AUTH" "${API_BASE}/tasks/${task}" | jq -r '.state')
    case "$state" in
      FINISHED)
        echo "region ${region}: ${what} task ${task} FINISHED"
        return 0 ;;
      ERROR)
        echo "::error::region ${region}: ${what} task ${task} state ERROR" >&2
        return 1 ;;
      *)
        echo "region ${region}: task ${task} state=${state} (attempt $((attempt + 1))/${max})"
        "$SLEEP" "$POLL_INTERVAL_SECS" ;;
    esac
    attempt=$((attempt + 1))
  done
  echo "::error::region ${region}: ${what} task ${task} did not finish after ${max} attempts" >&2
  return 1
}

# delete_image REGION IMAGE_ID -> remove an image we must not keep. Gcore deletes are
# task-based, so poll the returned task: that is what lets the log say the image is gone
# rather than merely accepted. Capture body AND status (not -f) so a rejection surfaces.
delete_image() {
  local region="$1" image="$2" resp http task
  resp=$("$CURL" -sS -w $'\n%{http_code}' -X DELETE -H "$AUTH" \
    "${API_BASE}/images/${GCORE_PROJECT_ID}/${region}/${image}")
  http="${resp##*$'\n'}"
  resp="${resp%$'\n'*}"
  if [ "$http" = "404" ]; then
    echo "region ${region}: image ${image} already gone (404)"
    return 0
  fi
  if [ "$http" -lt 200 ] || [ "$http" -ge 300 ]; then
    echo "::error::region ${region}: delete of image ${image} failed: HTTP ${http}: ${resp}" >&2
    return 1
  fi
  task=$(printf '%s' "$resp" | jq -r '.tasks[0] // empty')
  if [ -z "$task" ]; then
    echo "::warning::region ${region}: delete of image ${image} returned no task id (HTTP ${http}) — accepted, but completion is unconfirmed" >&2
    return 0
  fi
  poll_task "$region" "$task" "delete" "$DELETE_POLL_ATTEMPTS"
}

# fetch_field URL JQ_FILTER WHAT -> print the value, retrying an errored or empty result
# up to VISIBILITY_ATTEMPTS times. Used only by the visibility check, which fails closed:
# the bounded retry keeps one API blip from failing a good build without going back to
# assuming success. Diagnostics to stderr so stdout stays just the value.
fetch_field() {
  local url="$1" filter="$2" what="$3" attempt=0 out
  while [ "$attempt" -lt "$VISIBILITY_ATTEMPTS" ]; do
    attempt=$((attempt + 1))
    if out=$("$CURL" -sS -f -H "$AUTH" "$url" | jq -r "$filter") && [ -n "$out" ]; then
      printf '%s' "$out"
      return 0
    fi
    if [ "$attempt" -lt "$VISIBILITY_ATTEMPTS" ]; then
      echo "  ${what} lookup failed (attempt ${attempt}/${VISIBILITY_ATTEMPTS}); retrying" >&2
      "$SLEEP" "$POLL_INTERVAL_SECS"
    fi
  done
  return 1
}

# report_visibility REGION TASK_ID -> check the imported image's visibility: "private"
# (this project), "shared" (plus member projects, which gcore has no API to add) or
# "public" (every gcore customer). Only public is an exposure, and since gcore cannot
# *set* the field, deleting is the only remediation — public returns 1 either way, since
# the region ends up with no usable image. "shared" only warns; prune still collects it.
#
# Both lookups fail the publish once retries are exhausted. This is the only check that
# catches an image reaching gcore's global catalog, so "could not tell" must not pass as
# "not public" — the failure is what tells a human to go look.
report_visibility() {
  local region="$1" task="$2" image vis
  if ! image=$(fetch_field "${API_BASE}/tasks/${task}" \
      '.created_resources.images[0] // empty' "imported image ID from task ${task}"); then
    echo "::error::region ${region}: could not resolve the imported image ID from task ${task} after ${VISIBILITY_ATTEMPTS} attempts, so its visibility could not be checked. The import itself finished, so an image may exist and may be public — find it by name (${IMAGE_NAME}) in region ${region} and check it by hand." >&2
    return 1
  fi
  if ! vis=$(fetch_field "${API_BASE}/images/${GCORE_PROJECT_ID}/${region}/${image}" \
      '.visibility // empty' "visibility of image ${image}"); then
    echo "::error::region ${region}: could not read visibility of image ${image} after ${VISIBILITY_ATTEMPTS} attempts, so it is unknown whether it landed in gcore's public catalog — treat it as exposed and check it by hand." >&2
    return 1
  fi
  case "$vis" in
    private)
      echo "region ${region}: image ${image} visibility=private" ;;
    public)
      echo "::error::region ${region}: image ${image} visibility=public — in gcore's global catalog, not private to project ${GCORE_PROJECT_ID}. Deleting it." >&2
      if delete_image "$region" "$image"; then
        echo "region ${region}: public image ${image} removed; nothing was published to this region"
      else
        echo "::error::region ${region}: public image ${image} could NOT be deleted and is still in gcore's global catalog — delete it by hand" >&2
      fi
      return 1 ;;
    *)
      echo "::warning::region ${region}: image ${image} visibility=${vis}, not private — gcore has no API to change it. It is not in the public catalog, and prune still collects it." >&2 ;;
  esac
}

# Imports run concurrently: the *waiting* was the serial part, since draining each task
# before starting the next made wall-clock the SUM of the imports. Start them all, then
# poll every outstanding task per round with one sleep. Wall-clock becomes the slowest
# single region, which also shortens the qcow2's public window. Plain indexed arrays, no
# associative ones, so this still runs under bash 3.2.
main() {
  echo "Publishing ${IMAGE_NAME} to Gcore regions: ${GCORE_REGIONS}"
  local failures=0 region task attempt=0 pending i state
  local raw=() regions=() tasks=() states=()
  IFS=',' read -ra raw <<< "$GCORE_REGIONS"

  # Phase 1 — start every import. downloadimage returns a task id immediately, so these
  # POSTs are quick; issuing them sequentially also keeps them naturally spaced rather
  # than arriving as one burst.
  for region in "${raw[@]}"; do
    region="${region//[[:space:]]/}"
    [ -z "$region" ] && continue
    # Gcore reaps a task not STARTED within 10min of its own created_on, so POSTing all
    # ~30 imports in one window gives them a shared deadline the scheduler cannot meet.
    # Before the POST, and only once a task exists: no trailing wait, and a rejected POST
    # created nothing to space from.
    if [ "${#tasks[@]}" -gt 0 ] && [ "$IMPORT_STAGGER_SECS" -gt 0 ]; then
      "$SLEEP" "$IMPORT_STAGGER_SECS"
    fi
    echo "==> Importing ${IMAGE_NAME} into region ${region}"
    if ! task=$(import_region "$region"); then
      failures=$((failures + 1))
      continue
    fi
    regions+=("$region")
    tasks+=("$task")
    states+=("PENDING")
  done

  # Phase 2 — wait on all of them at once. One sleep per round, not per region.
  pending=${#regions[@]}
  while [ "$pending" -gt 0 ] && [ "$attempt" -lt "$POLL_ATTEMPTS" ]; do
    for i in "${!regions[@]}"; do
      [ "${states[$i]}" = "PENDING" ] || continue
      # A transient API error leaves state empty, which falls through to the retry
      # branch — the same self-healing the sequential version had.
      state=$("$CURL" -sS -f -H "$AUTH" "${API_BASE}/tasks/${tasks[$i]}" | jq -r '.state') || state=""
      case "$state" in
        FINISHED)
          echo "region ${regions[$i]}: import task ${tasks[$i]} FINISHED"
          states[i]=FINISHED
          pending=$((pending - 1))
          if ! report_visibility "${regions[$i]}" "${tasks[$i]}"; then
            failures=$((failures + 1))
          fi ;;
        ERROR)
          echo "::error::region ${regions[$i]}: import task ${tasks[$i]} state ERROR" >&2
          states[i]=ERROR
          pending=$((pending - 1))
          failures=$((failures + 1)) ;;
        *)
          echo "region ${regions[$i]}: task ${tasks[$i]} state=${state:-unknown} (attempt $((attempt + 1))/${POLL_ATTEMPTS})" ;;
      esac
    done
    attempt=$((attempt + 1))
    # An explicit if, not a `&&` chain: a false chain here would be a failing last
    # command in the loop body and `set -e` would kill the script.
    if [ "$pending" -gt 0 ] && [ "$attempt" -lt "$POLL_ATTEMPTS" ]; then
      "$SLEEP" "$POLL_INTERVAL_SECS"
    fi
  done

  # Whatever is still PENDING ran out of attempts.
  for i in "${!regions[@]}"; do
    if [ "${states[$i]}" = "PENDING" ]; then
      echo "::error::region ${regions[$i]}: import task ${tasks[$i]} did not finish after ${POLL_ATTEMPTS} attempts" >&2
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
