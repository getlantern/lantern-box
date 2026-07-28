#!/usr/bin/env bash
#
# Manage the dedicated gcore object storage that stages the lantern-box qcow2 for
# image import. gcore ingests images only by fetching from a URL, and its
# importer does an *unauthenticated* HEAD then GET — there is no API field to
# hand it credentials, and a presigned/SigV4 URL is bound to a single HTTP
# method (its HEAD preflight would 403). So the qcow2 has to be reachable with no
# credentials at all. We stage it in a throwaway, *randomly-named* bucket that is
# made briefly public-read (the anonymous-read bucket policy is applied by the
# AWS CLI in the workflow) for the duration of the import, then torn down. The
# random name keeps the object unguessable during that short window, and the
# bucket holds only that one qcow2.
#
# This script owns the gcore *control plane* (storage instance + buckets + access
# keys). The object upload, the public bucket policy, and the object delete are
# done by the AWS CLI against gcore's S3 endpoint in the workflow.
#
# Subcommands:
#   provision  Find-or-create the dedicated storage instance, sweep any leftover
#              stage buckets from a crashed run, create a fresh randomly-named
#              bucket, mint an ephemeral access key, and print these key=value
#              lines to stdout:
#                storage_id=<int>
#                region=<s3 region = location technical_name, e.g. s-ed1>
#                endpoint=<s3 endpoint, e.g. https://s-ed1.cloud.gcore.lu>
#                bucket=<name, e.g. lantern-box-stage-ab12cd34ef56ab78>
#                access_key=<key>
#                secret_key=<secret>
#   cleanup    Delete the stage bucket and the access key (best-effort; never
#              fails the job). Reads STORAGE_ID, BUCKET, and ACCESS_KEY from env.
#
# Required env: GCORE_API_KEY
# Config env (defaults): STORAGE_NAME=lantern-box-images,
#   LOCATION_NAME=luxembourg-2, BUCKET_PREFIX=lantern-box-stage-
# Test/override env: CURL=curl, SLEEP=sleep,
#   GCORE_STORAGE_API_BASE=https://api.gcore.com/storage/v4,
#   POLL_ATTEMPTS=60, POLL_INTERVAL_SECS=5
set -euo pipefail

: "${GCORE_API_KEY:?GCORE_API_KEY must be set}"

CURL="${CURL:-curl}"
SLEEP="${SLEEP:-sleep}"
API_BASE="${GCORE_STORAGE_API_BASE:-https://api.gcore.com/storage/v4}"
AUTH="Authorization: APIKey ${GCORE_API_KEY}"
STORAGE_NAME="${STORAGE_NAME:-lantern-box-images}"
LOCATION_NAME="${LOCATION_NAME:-luxembourg-2}"
# Each run stages into a fresh bucket named "<prefix><random>"; the prefix scopes
# the stale-bucket sweep so the instance's other buckets are never touched.
BUCKET_PREFIX="${BUCKET_PREFIX:-lantern-box-stage-}"
POLL_ATTEMPTS="${POLL_ATTEMPTS:-60}"
POLL_INTERVAL_SECS="${POLL_INTERVAL_SECS:-5}"
# gcore's S3 API endpoint is https://<region>.<suffix>, where <region> is the
# location's technical_name (luxembourg-2 -> s-ed1). NOTE: this is NOT the
# object_storages `address` field — that host is not the S3 API endpoint.
S3_ENDPOINT_SUFFIX="${GCORE_S3_ENDPOINT_SUFFIX:-cloud.gcore.lu}"

# GET with fail-on-HTTP-error. A real API error propagates (pipefail); a valid
# empty result does not.
api_get() { "$CURL" -sS -f -H "$AUTH" "$@"; }

# find_storage -> "<id>\t<provisioning_status>" for the instance named
# STORAGE_NAME, or empty output if absent. Uses jq indexing (not `head`) so it
# never triggers a SIGPIPE under `set -o pipefail`.
find_storage() {
  api_get "${API_BASE}/object_storages?limit=1000" | jq -r --arg n "$STORAGE_NAME" '
    [.results[] | select(.name == $n)][0]
    | if . then "\(.id)\t\(.provisioning_status)" else empty end'
}

# resolve_region -> the S3 region code (the location's technical_name) for
# LOCATION_NAME, e.g. luxembourg-2 -> s-ed1. Empty output if not found.
resolve_region() {
  api_get "${API_BASE}/locations?limit=1000" | jq -r --arg n "$LOCATION_NAME" '
    [.results[] | select(.name == $n)][0].technical_name // empty'
}

# storage_field ID FIELD -> a single field from the instance record.
storage_field() {
  api_get "${API_BASE}/object_storages/$1" | jq -r --arg f "$2" '.[$f]'
}

# wait_active ID -> returns when provisioning_status == active; fails on a
# terminal state or timeout. Diagnostics go to stderr.
wait_active() {
  local id="$1" attempt=0 status
  while [ "$attempt" -lt "$POLL_ATTEMPTS" ]; do
    status=$(storage_field "$id" provisioning_status)
    case "$status" in
      active) return 0 ;;
      deleting | deleted) echo "::error::gcore storage ${id} in terminal state ${status}" >&2; return 1 ;;
      *) "$SLEEP" "$POLL_INTERVAL_SECS" ;;
    esac
    attempt=$((attempt + 1))
  done
  echo "::error::gcore storage ${id} not active after ${POLL_ATTEMPTS} attempts" >&2
  return 1
}

# ensure_storage -> "<id>"; creates the instance and waits for active if it
# doesn't already exist. Diagnostics go to stderr so stdout stays clean.
ensure_storage() {
  local line id status resp
  line=$(find_storage)
  if [ -n "$line" ]; then
    IFS=$'\t' read -r id status <<<"$line"
    [ "$status" = "active" ] || wait_active "$id" || return 1
  else
    echo "creating gcore storage ${STORAGE_NAME} in ${LOCATION_NAME}" >&2
    resp=$("$CURL" -sS -f -X POST -H "$AUTH" -H 'Content-Type: application/json' \
      -d "$(jq -n --arg n "$STORAGE_NAME" --arg loc "$LOCATION_NAME" '{name: $n, location_name: $loc}')" \
      "${API_BASE}/object_storages")
    id=$(printf '%s' "$resp" | jq -r '.id')
    wait_active "$id" || return 1
  fi
  printf '%s' "$id"
}

# rand_suffix -> 16 lowercase hex chars from /dev/urandom. Reads a fixed number
# of bytes with `od` (no `head`) so it can't trip SIGPIPE under set -o pipefail.
rand_suffix() { od -An -N8 -tx1 /dev/urandom | tr -d ' \n'; }

# sweep_stale_buckets ID -> best-effort delete of any leftover stage buckets from
# a crashed run whose cleanup never ran. Only touches buckets whose name starts
# with BUCKET_PREFIX, so the dedicated instance's other buckets are untouched.
# (Control-plane delete generally requires the bucket be empty; a leftover that
# still holds an object is left for the next run / operator — see README.)
sweep_stale_buckets() {
  local id="$1" b
  for b in $(api_get "${API_BASE}/object_storages/${id}/buckets?limit=1000" \
    | jq -r --arg p "$BUCKET_PREFIX" '.results[] | select(.name | startswith($p)) | .name'); do
    echo "sweeping stale stage bucket ${b} on storage ${id}" >&2
    "$CURL" -sS -f -X DELETE -H "$AUTH" \
      "${API_BASE}/object_storages/${id}/buckets/${b}" >/dev/null 2>&1 || true
  done
}

# create_bucket ID NAME -> creates the named bucket.
create_bucket() {
  local id="$1" name="$2"
  "$CURL" -sS -f -X POST -H "$AUTH" -H 'Content-Type: application/json' \
    -d "$(jq -n --arg b "$name" '{name: $b}')" \
    "${API_BASE}/object_storages/${id}/buckets" >/dev/null
}

# prune_keys ID -> delete every existing access key. Safe because the instance is
# dedicated to this pipeline; recovers the max-2-keys-per-storage cap after a run
# whose cleanup didn't run.
prune_keys() {
  local id="$1" k
  for k in $(api_get "${API_BASE}/object_storages/${id}/access_keys?limit=1000" | jq -r '.results[].access_key'); do
    echo "pruning stale access key on storage ${id}" >&2
    "$CURL" -sS -f -X DELETE -H "$AUTH" "${API_BASE}/object_storages/${id}/access_keys/${k}" >/dev/null || true
  done
}

# mint_key ID -> "<access_key>\t<secret_key>"
mint_key() {
  local id="$1" resp
  resp=$("$CURL" -sS -f -X POST -H "$AUTH" "${API_BASE}/object_storages/${id}/access_keys")
  printf '%s\t%s' \
    "$(printf '%s' "$resp" | jq -r '.access_key')" \
    "$(printf '%s' "$resp" | jq -r '.secret_key')"
}

provision() {
  local id region endpoint bucket out ak sk
  id=$(ensure_storage) || { echo "::error::failed to resolve gcore storage" >&2; exit 1; }
  [ -n "$id" ] || { echo "::error::empty gcore storage id" >&2; exit 1; }
  region=$(resolve_region || true)
  [ -n "$region" ] || { echo "::error::could not resolve S3 region (technical_name) for location ${LOCATION_NAME}" >&2; exit 1; }
  endpoint="https://${region}.${S3_ENDPOINT_SUFFIX}"
  sweep_stale_buckets "$id"
  bucket="${BUCKET_PREFIX}$(rand_suffix)"
  create_bucket "$id" "$bucket"
  prune_keys "$id"
  out=$(mint_key "$id")
  IFS=$'\t' read -r ak sk <<<"$out"
  [ -n "$ak" ] && [ -n "$sk" ] || { echo "::error::failed to mint gcore access key" >&2; exit 1; }
  printf 'storage_id=%s\nregion=%s\nendpoint=%s\nbucket=%s\naccess_key=%s\nsecret_key=%s\n' \
    "$id" "$region" "$endpoint" "$bucket" "$ak" "$sk"
}

cleanup() {
  : "${STORAGE_ID:?STORAGE_ID must be set for cleanup}"
  : "${ACCESS_KEY:?ACCESS_KEY must be set for cleanup}"
  # Delete the stage bucket (and thus its public policy). The workflow removes the
  # staged object just before calling this, so the bucket is empty by now.
  if [ -n "${BUCKET:-}" ]; then
    "$CURL" -sS -f -X DELETE -H "$AUTH" \
      "${API_BASE}/object_storages/${STORAGE_ID}/buckets/${BUCKET}" >/dev/null 2>&1 \
      && echo "deleted gcore stage bucket ${BUCKET} on storage ${STORAGE_ID}" \
      || echo "gcore stage bucket ${BUCKET} already gone or delete failed (ignored)"
  fi
  "$CURL" -sS -f -X DELETE -H "$AUTH" \
    "${API_BASE}/object_storages/${STORAGE_ID}/access_keys/${ACCESS_KEY}" >/dev/null 2>&1 \
    && echo "deleted gcore access key on storage ${STORAGE_ID}" \
    || echo "gcore access key already gone or delete failed (ignored)"
}

case "${1:-}" in
  provision) provision ;;
  cleanup) cleanup ;;
  *)
    echo "usage: $0 {provision|cleanup}" >&2
    exit 2
    ;;
esac
