#!/usr/bin/env bash
#
# Manage the dedicated gcore object storage that stages the lantern-box qcow2 for
# image import. gcore ingests images only by URL, and its importer does an
# *unauthenticated* HEAD then GET: there is no API field for source credentials,
# and a presigned SigV4 URL is bound to one HTTP method (its HEAD preflight 403s).
# So the qcow2 is staged in a throwaway, randomly-named bucket made briefly
# public-read for the import, then torn down.
#
# This script owns the gcore *control plane* (storage instance + buckets + access
# keys); the upload, the public bucket policy, and the object delete are done by
# the AWS CLI against gcore's S3 endpoint in the workflow. The one place this
# script reaches for the S3 API too is the stale-bucket sweep, which has to revoke
# the policy and empty a leftover before the control plane will delete it.
#
# Subcommands:
#   provision  Find-or-create the storage instance, mint an ephemeral access key,
#              sweep leftover stage buckets with it, create a fresh randomly-named
#              bucket with a 1-day object-expiry rule, and print these key=value
#              lines to stdout:
#                storage_id=<int>
#                region=<s3 region = location technical_name, e.g. s-ed1>
#                endpoint=<s3 endpoint, e.g. https://s-ed1.cloud.gcore.lu>
#                bucket=<name, e.g. lantern-box-stage-ab12cd34ef56ab78>
#                access_key=<key>
#                secret_key=<secret>
#   cleanup    Delete the stage bucket and the access key. Reads STORAGE_ID,
#              BUCKET, and ACCESS_KEY from env. Fails if the bucket survives the
#              delete, since that means the staged qcow2 is still sitting there.
#   empty-bucket
#              Abort incomplete multipart uploads, then remove every object. Reads
#              BUCKET and ENDPOINT plus AWS_* credentials from env. Called by the
#              workflow's teardown and reused by provision's stale-bucket sweep, so
#              both paths empty a bucket the same way.
#
# Required env: GCORE_API_KEY
# Config env (defaults): STORAGE_NAME=lantern-box-images,
#   LOCATION_NAME=luxembourg-2, BUCKET_PREFIX=lantern-box-stage-
# Test/override env: CURL=curl, AWS=aws, SLEEP=sleep,
#   GCORE_STORAGE_API_BASE=https://api.gcore.com/storage/v4,
#   POLL_ATTEMPTS=60, POLL_INTERVAL_SECS=5
set -euo pipefail

: "${GCORE_API_KEY:?GCORE_API_KEY must be set}"

CURL="${CURL:-curl}"
AWS="${AWS:-aws}"
SLEEP="${SLEEP:-sleep}"
API_BASE="${GCORE_STORAGE_API_BASE:-https://api.gcore.com/storage/v4}"
AUTH="Authorization: APIKey ${GCORE_API_KEY}"
STORAGE_NAME="${STORAGE_NAME:-lantern-box-images}"
LOCATION_NAME="${LOCATION_NAME:-luxembourg-2}"
# Each run stages into a fresh "<prefix><random>" bucket; the prefix scopes the
# stale-bucket sweep so the instance's other buckets are never touched.
BUCKET_PREFIX="${BUCKET_PREFIX:-lantern-box-stage-}"
POLL_ATTEMPTS="${POLL_ATTEMPTS:-60}"
POLL_INTERVAL_SECS="${POLL_INTERVAL_SECS:-5}"
# gcore's S3 API endpoint is https://<region>.<suffix>, where <region> is the
# location's technical_name (luxembourg-2 -> s-ed1). NOT the object_storages
# `address` field — that host is not the S3 API endpoint.
S3_ENDPOINT_SUFFIX="${GCORE_S3_ENDPOINT_SUFFIX:-cloud.gcore.lu}"

# GET with fail-on-HTTP-error: a real API error propagates (pipefail), a valid
# empty result does not.
api_get() { "$CURL" -sS -f -H "$AUTH" "$@"; }

# find_storage -> "<id>\t<provisioning_status>" for STORAGE_NAME, or empty if
# absent. jq indexing rather than `head`, which would SIGPIPE under pipefail.
find_storage() {
  api_get "${API_BASE}/object_storages?limit=1000" | jq -r --arg n "$STORAGE_NAME" '
    [.results[] | select(.name == $n)][0]
    | if . then "\(.id)\t\(.provisioning_status)" else empty end'
}

# resolve_region -> the S3 region code (LOCATION_NAME's technical_name), e.g.
# luxembourg-2 -> s-ed1. Empty if not found.
resolve_region() {
  api_get "${API_BASE}/locations?limit=1000" | jq -r --arg n "$LOCATION_NAME" '
    [.results[] | select(.name == $n)][0].technical_name // empty'
}

# storage_field ID FIELD -> a single field from the instance record.
storage_field() {
  api_get "${API_BASE}/object_storages/$1" | jq -r --arg f "$2" '.[$f]'
}

# wait_active ID -> returns when provisioning_status == active; fails on a terminal
# state or timeout.
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

# ensure_storage -> "<id>"; creates the instance and waits for active if absent.
# Diagnostics go to stderr so stdout stays clean.
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

# rand_suffix -> 16 lowercase hex chars. Fixed-size `od` read rather than `head`,
# which would SIGPIPE under pipefail.
rand_suffix() { od -An -N8 -tx1 /dev/urandom | tr -d ' \n'; }

# sweep_stale_buckets ID ENDPOINT -> best-effort teardown of stage buckets left by
# a run that died before its own cleanup (a lost runner; a cancelled job still runs
# its always() teardown). Only BUCKET_PREFIX-named buckets, so the instance's
# others are untouched. Order matters: revoke the anonymous-read policy FIRST —
# that is what ends the exposure, and it works whether or not the rest succeeds —
# then empty the bucket, because the control-plane delete refuses a non-empty one.
# Expects AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_DEFAULT_REGION in the env.
sweep_stale_buckets() {
  local id="$1" endpoint="$2" b
  for b in $(api_get "${API_BASE}/object_storages/${id}/buckets?limit=1000" \
    | jq -r --arg p "$BUCKET_PREFIX" '.results[] | select(.name | startswith($p)) | .name'); do
    echo "sweeping stale stage bucket ${b} on storage ${id}" >&2
    "$AWS" s3api delete-bucket-policy --bucket "$b" --endpoint-url "$endpoint" >/dev/null 2>&1 \
      && echo "  revoked anonymous-read policy on ${b}" >&2 \
      || echo "  no bucket policy to revoke on ${b} (or the revoke failed)" >&2
    empty_bucket "$b" "$endpoint"
    "$CURL" -sS -f -X DELETE -H "$AUTH" \
      "${API_BASE}/object_storages/${id}/buckets/${b}" >/dev/null 2>&1 \
      && echo "  deleted ${b}" >&2 \
      || echo "::warning::stale stage bucket ${b} on storage ${id} could not be deleted; its public policy was revoked, so this is cost rather than exposure" >&2
  done
}

# bucket_exists ID NAME -> 0 present, 1 absent, 2 could not tell. Lets cleanup tell
# "the delete raced something that already removed it" apart from "the delete was
# refused and the bucket is still there". The third state matters: folding an
# unreadable listing into "absent" would report a stranded bucket as cleaned up,
# which is the exact failure this check exists to catch. So the fetch and the parse
# are separated — api_get uses curl -f, whose non-zero covers auth, 5xx and network
# alike, and jq -e exits 1 only for a listing it read and found no match in.
bucket_exists() {
  local listing rc=0
  listing=$(api_get "${API_BASE}/object_storages/$1/buckets?limit=1000") || return 2
  jq -e --arg b "$2" 'any(.results[]; .name == $b)' >/dev/null <<<"$listing" || rc=$?
  case "$rc" in
    0) return 0 ;;
    1) return 1 ;;
    *) return 2 ;;
  esac
}

# set_bucket_expiry BUCKET ENDPOINT -> attach a 1-day object-expiry lifecycle rule.
# This is the last-resort backstop: if a run dies so hard that no teardown runs at all
# (a lost runner — a cancelled job still runs its always() steps), gcore deletes the
# staged qcow2 on its own. That also un-sticks the bucket, because the control-plane
# delete refuses a non-empty one. Gcore's expiry pass runs around midnight UTC and
# lands a day later than Days suggests, so the worst case is ~48h — a floor under the
# exposure, never a substitute for the workflow's policy revoke, which closes the
# window in seconds. Gcore implements the legacy put-bucket-lifecycle, not
# put-bucket-lifecycle-configuration.
set_bucket_expiry() {
  local bucket="$1" endpoint="$2"
  "$AWS" s3api put-bucket-lifecycle --bucket "$bucket" --endpoint-url "$endpoint" \
    --lifecycle-configuration "$(jq -nc '{Rules: [{
      ID: "expire_stage_qcow2", Prefix: "", Status: "Enabled", Expiration: {Days: 1}
    }]}')" >/dev/null
}

# empty_bucket BUCKET ENDPOINT -> remove everything the control-plane delete counts.
# Incomplete multipart uploads come first: the qcow2 is multi-GB so `aws s3 cp` uploads
# it in parts, and a run killed mid-upload leaves parts that `s3 rm` does NOT touch and
# that the expiry rule does not cover either (that needs AbortIncompleteMultipartUpload,
# which gcore does not document). Skipping them is how a bucket becomes permanently
# un-deletable. Best-effort throughout: a bucket that was never written to has nothing
# to remove, and that is not an error.
empty_bucket() {
  local bucket="$1" endpoint="$2" mpu key uploadid
  mpu=$("$AWS" s3api list-multipart-uploads --bucket "$bucket" --endpoint-url "$endpoint" \
    --query 'Uploads[].[Key,UploadId]' --output text 2>/dev/null || true)
  if [ -n "$mpu" ]; then
    while IFS=$'\t' read -r key uploadid; do
      # An absent Uploads key renders as "None" with no second column; skip it.
      [ -n "${uploadid:-}" ] || continue
      echo "  aborting incomplete multipart upload ${uploadid} for ${key} in ${bucket}" >&2
      "$AWS" s3api abort-multipart-upload --bucket "$bucket" --key "$key" \
        --upload-id "$uploadid" --endpoint-url "$endpoint" >/dev/null 2>&1 \
        || echo "::warning::could not abort multipart upload ${uploadid} for ${key} in ${bucket}" >&2
    done <<<"$mpu"
  fi
  "$AWS" s3 rm "s3://${bucket}" --recursive --endpoint-url "$endpoint" >/dev/null 2>&1 \
    || echo "  nothing to remove from ${bucket} (or the remove failed)" >&2
}

# create_bucket ID NAME -> creates the named bucket.
create_bucket() {
  local id="$1" name="$2"
  "$CURL" -sS -f -X POST -H "$AUTH" -H 'Content-Type: application/json' \
    -d "$(jq -n --arg b "$name" '{name: $b}')" \
    "${API_BASE}/object_storages/${id}/buckets" >/dev/null
}

# prune_keys ID -> delete every existing access key. Safe because the instance is
# dedicated to this pipeline; recovers the max-2-keys cap after a crashed run.
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
  prune_keys "$id"
  out=$(mint_key "$id")
  IFS=$'\t' read -r ak sk <<<"$out"
  [ -n "$ak" ] && [ -n "$sk" ] || { echo "::error::failed to mint gcore access key" >&2; exit 1; }
  # Sweep with the key in hand — emptying a leftover needs S3 credentials — and
  # before creating this run's bucket, which shares BUCKET_PREFIX and would
  # otherwise be a sweep target itself. Subshell so the credentials never leak into
  # the rest of provision; best-effort so a sweep failure can't block the build.
  (
    # shellcheck disable=SC2030 # Subshell-local is the point: these must not outlive
    # the block. Same below.
    export AWS_ACCESS_KEY_ID="$ak" AWS_SECRET_ACCESS_KEY="$sk" AWS_DEFAULT_REGION="$region"
    sweep_stale_buckets "$id" "$endpoint"
  ) || echo "::warning::stale stage-bucket sweep did not complete" >&2
  bucket="${BUCKET_PREFIX}$(rand_suffix)"
  create_bucket "$id" "$bucket"
  # Attach the expiry backstop before anything is uploaded, so it covers a run that
  # dies at any later point. A warning rather than a failure: the primary controls
  # (policy revoke, object delete, sweep) are what contain the exposure.
  (
    # shellcheck disable=SC2031 # Deliberately scoped to this subshell, as above.
    export AWS_ACCESS_KEY_ID="$ak" AWS_SECRET_ACCESS_KEY="$sk" AWS_DEFAULT_REGION="$region"
    set_bucket_expiry "$bucket" "$endpoint"
  ) || echo "::warning::could not attach the 1-day expiry rule to ${bucket}; teardown is the only cleanup for this run" >&2
  printf 'storage_id=%s\nregion=%s\nendpoint=%s\nbucket=%s\naccess_key=%s\nsecret_key=%s\n' \
    "$id" "$region" "$endpoint" "$bucket" "$ak" "$sk"
}

cleanup() {
  : "${STORAGE_ID:?STORAGE_ID must be set for cleanup}"
  : "${ACCESS_KEY:?ACCESS_KEY must be set for cleanup}"
  local stuck=0 exists=0
  # The workflow revokes the bucket policy and deletes the staged object before
  # calling this, so the bucket should be empty and already non-public. If the
  # delete is refused and the bucket is still there, it is still holding the qcow2:
  # report that instead of swallowing it, because a silently-ignored failure here is
  # how a stage bucket gets stranded with nothing watching it.
  #
  # The bucket NAME is ::add-mask::ed in CI, so it renders as *** in these messages —
  # hence the prefix-and-instance hint, which is what an operator can actually act on.
  if [ -n "${BUCKET:-}" ]; then
    if "$CURL" -sS -f -X DELETE -H "$AUTH" \
      "${API_BASE}/object_storages/${STORAGE_ID}/buckets/${BUCKET}" >/dev/null 2>&1; then
      echo "deleted gcore stage bucket ${BUCKET} on storage ${STORAGE_ID}"
    else
      bucket_exists "$STORAGE_ID" "$BUCKET" || exists=$?
      case "$exists" in
        1)
          echo "gcore stage bucket ${BUCKET} already gone" ;;
        0)
          stuck=1
          echo "::error::gcore stage bucket ${BUCKET} on storage ${STORAGE_ID} could not be deleted and still exists — it is probably still holding the staged qcow2. Its public policy was revoked earlier in the job, so this is cost rather than exposure; the next run's sweep empties and deletes it. To find it by hand, list ${BUCKET_PREFIX}* buckets on storage instance ${STORAGE_ID}." >&2 ;;
        *)
          # Unknown, so assume the worst: the delete was refused and we cannot even
          # read the listing to see whether the bucket survived it.
          stuck=1
          echo "::error::gcore stage bucket ${BUCKET} on storage ${STORAGE_ID} could not be deleted, and the bucket listing could not be read to confirm whether it survived — treat it as still present and holding the staged qcow2. Its public policy was revoked earlier in the job. Check ${BUCKET_PREFIX}* buckets on storage instance ${STORAGE_ID}; the next run's sweep also empties and deletes them." >&2 ;;
      esac
    fi
  fi
  # Drop the key even when the bucket is stuck: it only grants write access to a
  # bucket already being left behind, and the sweep that reclaims that bucket mints
  # a fresh key of its own.
  "$CURL" -sS -f -X DELETE -H "$AUTH" \
    "${API_BASE}/object_storages/${STORAGE_ID}/access_keys/${ACCESS_KEY}" >/dev/null 2>&1 \
    && echo "deleted gcore access key on storage ${STORAGE_ID}" \
    || echo "gcore access key already gone or delete failed (ignored)"
  return "$stuck"
}

case "${1:-}" in
  provision) provision ;;
  cleanup) cleanup ;;
  empty-bucket)
    : "${BUCKET:?BUCKET must be set for empty-bucket}"
    : "${ENDPOINT:?ENDPOINT must be set for empty-bucket}"
    empty_bucket "$BUCKET" "$ENDPOINT"
    ;;
  *)
    echo "usage: $0 {provision|cleanup|empty-bucket}" >&2
    exit 2
    ;;
esac
