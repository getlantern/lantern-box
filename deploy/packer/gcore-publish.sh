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
#   POLL_ATTEMPTS (default 360) — polls allowed PER REGION PER ATTEMPT, not a budget
#   shared across regions. A region only consumes polls while it holds a slot, and
#   gcore's observed import tail reaches ~75min, so this is 90min at the default
#   interval rather than the 30min that used to reap healthy imports.
#   POLL_INTERVAL_SECS (default 15),
#   MAX_INFLIGHT (default 6) — how many imports may be outstanding at once. THE
#   important knob: gcore fails any task it has not STARTED within 10min of that
#   task's own created_on, so a task must not be created until gcore can plausibly
#   start it. See main().
#   IMPORT_ATTEMPTS (default 2) — import tries per region before giving up on it.
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
POLL_ATTEMPTS="${POLL_ATTEMPTS:-360}"
POLL_INTERVAL_SECS="${POLL_INTERVAL_SECS:-15}"
MAX_INFLIGHT="${MAX_INFLIGHT:-6}"
IMPORT_ATTEMPTS="${IMPORT_ATTEMPTS:-2}"
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

# fetch_task TASK_ID -> print the task's raw JSON body. Non-zero on a transport
# failure, which callers treat as "state not known yet" and retry. The body is kept
# whole rather than reduced to .state so an ERROR can be reported verbatim: gcore's
# failure detail lives in fields this script deliberately does not enumerate.
fetch_task() {
  "$CURL" -sS -f -H "$AUTH" "${API_BASE}/tasks/${1}"
}

# dump_task_body WHAT BODY -> echo a gcore task body to stderr for a human. Printed
# in full, pretty-printed when it parses and raw when it does not, because guessing
# which field carries the reason is how the "scheduler cleanup" failures stayed
# invisible across two builds.
dump_task_body() {
  local what="$1" body="$2"
  if [ -z "$body" ]; then
    echo "  ${what}: gcore returned no body" >&2
    return 0
  fi
  echo "  ${what}: gcore task body follows" >&2
  # Captured rather than piped straight to stderr: `jq ... 2>/dev/null >&2` points
  # stdout at whatever fd2 already is, which by then is /dev/null — the body vanished.
  local pretty
  if pretty=$(printf '%s\n' "$body" | jq . 2>/dev/null) && [ -n "$pretty" ]; then
    printf '%s\n' "$pretty" >&2
  else
    printf '%s\n' "$body" >&2
  fi
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

# Per-region bookkeeping as parallel indexed arrays (no associative arrays, so this
# still runs under bash 3.2). Global rather than main()-local so retire_region can
# mutate them.
R_IDS=()     # region id
R_TASKS=()   # current task id, "" before the first POST
R_STATES=()  # QUEUED | INFLIGHT | DONE | FAILED
R_POLLS=()   # polls consumed by the current attempt
R_TRIES=()   # import attempts started
R_WHY=()     # why a FAILED region failed, for the summary

# retire_region INDEX REASON -> a region's attempt just ended badly. Requeue it when it
# still has an attempt left, otherwise mark it FAILED and record why. A requeue re-POSTs
# against the same already-hosted qcow2, so a retry costs one API call and no re-upload.
retire_region() {
  local i="$1" reason="$2"
  if [ "${R_TRIES[$i]}" -lt "$IMPORT_ATTEMPTS" ]; then
    echo "::warning::region ${R_IDS[$i]}: ${reason} — requeueing (attempt $((${R_TRIES[$i]} + 1))/${IMPORT_ATTEMPTS})" >&2
    R_STATES[i]=QUEUED
    R_TASKS[i]=""
    R_POLLS[i]=0
    return 0
  fi
  echo "::error::region ${R_IDS[$i]}: ${reason} — no attempts left (${R_TRIES[$i]}/${IMPORT_ATTEMPTS})" >&2
  R_STATES[i]=FAILED
  R_WHY[i]="$reason"
}

# summarize -> report which regions ended up with the image and which did not, then set
# the exit status. Landing in 27 of 29 regions is not the same event as landing nowhere,
# and the old all-or-nothing "N region import(s) failed" hid the difference — including
# from whoever has to decide whether production was actually affected.
summarize() {
  local i ok=() bad=() n_ok=0 n_bad=0
  for i in "${!R_IDS[@]}"; do
    if [ "${R_STATES[$i]}" = "DONE" ]; then
      ok+=("${R_IDS[$i]}")
      n_ok=$((n_ok + 1))
    else
      bad+=("${R_IDS[$i]}")
      n_bad=$((n_bad + 1))
    fi
  done
  echo
  echo "=== ${IMAGE_NAME} publish summary ==="
  echo "landed in ${n_ok}/${#R_IDS[@]} region(s): ${ok[*]:-none}"
  if [ "$n_bad" -eq 0 ]; then
    echo "All gcore region imports finished."
    return 0
  fi
  echo "MISSING the image in ${n_bad} region(s): ${bad[*]}"
  for i in "${!R_IDS[@]}"; do
    [ "${R_STATES[$i]}" = "DONE" ] && continue
    echo "  region ${R_IDS[$i]}: ${R_WHY[$i]:-did not complete}"
  done
  echo "::error::gcore publish left ${n_bad} of ${#R_IDS[@]} region(s) without ${IMAGE_NAME}: ${bad[*]}" >&2
  exit 1
}

# Imports run with BOUNDED concurrency, and that bound is the whole point.
#
# Gcore fails any task it has not STARTED within 10min of that task's own created_on,
# while its per-project scheduler runs only a few image imports at a time and each one
# takes ~10-75min. Creating all ~30 imports up front therefore guaranteed the tail of
# the queue was reaped before it ever ran: in both failed builds EVERY errored region
# came back with gcore's "Task was not started within 10 minutes of creation. Marked as
# failed by scheduler cleanup." Spacing the POSTs did not fix that, because a fixed
# stagger still lets the queue grow without limit.
#
# So a task is created only when a slot is free. At most MAX_INFLIGHT tasks exist at
# once, each is young when gcore picks it up, and the 10min deadline stops being a
# deadline we set ourselves up to miss. Polls are per-region and per-attempt: a region
# consumes them only while it holds a slot, so one slow region cannot spend a budget
# the others still need.
main() {
  echo "Publishing ${IMAGE_NAME} to Gcore regions: ${GCORE_REGIONS}"
  local raw=() region i next inflight remaining state body task submitted=0
  IFS=',' read -ra raw <<< "$GCORE_REGIONS"
  for region in "${raw[@]}"; do
    region="${region//[[:space:]]/}"
    [ -z "$region" ] && continue
    R_IDS+=("$region")
    R_TASKS+=("")
    R_STATES+=("QUEUED")
    R_POLLS+=("0")
    R_TRIES+=("0")
    R_WHY+=("")
  done
  if [ "${#R_IDS[@]}" -eq 0 ]; then
    echo "::error::GCORE_REGIONS contained no region IDs" >&2
    exit 1
  fi
  echo "  ${#R_IDS[@]} region(s); at most ${MAX_INFLIGHT} in flight; ${IMPORT_ATTEMPTS} attempt(s) each; ${POLL_ATTEMPTS} poll(s) x ${POLL_INTERVAL_SECS}s per attempt"

  while :; do
    inflight=0
    for i in "${!R_IDS[@]}"; do
      [ "${R_STATES[$i]}" = "INFLIGHT" ] && inflight=$((inflight + 1))
    done

    # Fill whatever slots are free.
    while [ "$inflight" -lt "$MAX_INFLIGHT" ]; do
      next=-1
      for i in "${!R_IDS[@]}"; do
        if [ "${R_STATES[$i]}" = "QUEUED" ]; then
          next="$i"
          break
        fi
      done
      [ "$next" -lt 0 ] && break
      # Spacing between POSTs, so a refill does not arrive as a burst. Before the POST
      # and only once one has already gone out: no leading wait, no trailing one, and
      # a refused POST left nothing to space from.
      if [ "$submitted" -gt 0 ] && [ "$IMPORT_STAGGER_SECS" -gt 0 ]; then
        "$SLEEP" "$IMPORT_STAGGER_SECS"
      fi
      R_TRIES[next]=$((${R_TRIES[$next]} + 1))
      submitted=$((submitted + 1))
      echo "==> Importing ${IMAGE_NAME} into region ${R_IDS[$next]} (attempt ${R_TRIES[$next]}/${IMPORT_ATTEMPTS}, $((inflight + 1))/${MAX_INFLIGHT} in flight)"
      if task=$(import_region "${R_IDS[$next]}"); then
        R_TASKS[next]="$task"
        R_STATES[next]=INFLIGHT
        R_POLLS[next]=0
        inflight=$((inflight + 1))
      else
        # The POST itself was refused. retire_region may requeue it, so break out and
        # let the round's sleep space the retry rather than spinning on it here.
        retire_region "$next" "import POST was refused"
        break
      fi
    done

    # One poll per outstanding task per round.
    for i in "${!R_IDS[@]}"; do
      [ "${R_STATES[$i]}" = "INFLIGHT" ] || continue
      # A transient API error leaves the body empty, which falls through to the retry
      # branch — the same self-healing the shared-budget version had.
      body=$(fetch_task "${R_TASKS[$i]}") || body=""
      state=""
      if [ -n "$body" ]; then
        state=$(printf '%s' "$body" | jq -r '.state // empty' 2>/dev/null) || state=""
      fi
      case "$state" in
        FINISHED)
          echo "region ${R_IDS[$i]}: import task ${R_TASKS[$i]} FINISHED"
          R_STATES[i]=DONE
          # An import that landed but cannot be shown to be non-public is not a
          # success, and re-importing would not change that, so it does not retry.
          if ! report_visibility "${R_IDS[$i]}" "${R_TASKS[$i]}"; then
            R_STATES[i]=FAILED
            R_WHY[i]="imported, but the visibility check failed"
          fi ;;
        ERROR)
          echo "::error::region ${R_IDS[$i]}: import task ${R_TASKS[$i]} state ERROR" >&2
          dump_task_body "region ${R_IDS[$i]}" "$body"
          retire_region "$i" "import task ${R_TASKS[$i]} reported ERROR" ;;
        *)
          R_POLLS[i]=$((${R_POLLS[$i]} + 1))
          if [ "${R_POLLS[$i]}" -ge "$POLL_ATTEMPTS" ]; then
            retire_region "$i" "import task ${R_TASKS[$i]} did not finish after ${POLL_ATTEMPTS} polls"
          else
            echo "region ${R_IDS[$i]}: task ${R_TASKS[$i]} state=${state:-unknown} (poll ${R_POLLS[$i]}/${POLL_ATTEMPTS})"
          fi ;;
      esac
    done

    # Done once nothing is in flight and nothing is still waiting for a slot.
    remaining=0
    for i in "${!R_IDS[@]}"; do
      case "${R_STATES[$i]}" in
        QUEUED | INFLIGHT) remaining=$((remaining + 1)) ;;
      esac
    done
    [ "$remaining" -eq 0 ] && break
    "$SLEEP" "$POLL_INTERVAL_SECS"
  done

  summarize
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main
fi
