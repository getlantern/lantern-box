#!/usr/bin/env bash
# Tests for gcore-storage.sh using fake `curl` and `aws` binaries on PATH (no
# network). The curl fake emulates the gcore object-storage control plane:
# list/create storages, get a storage, list/create/delete buckets, and
# list/create/delete access keys. The aws fake records the S3 calls the stale-bucket
# sweep makes, since the script redirects their output.
# Run: bash deploy/packer/gcore-storage.test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/gcore-storage.sh"
FAILED=0
TMP_DIRS=()

cleanup_tmp() {
  local d
  for d in "${TMP_DIRS[@]:-}"; do [ -n "$d" ] && rm -rf "$d"; done
}
trap cleanup_tmp EXIT

# Writes fake `curl` and `aws` binaries to a fresh temp dir (prepended to PATH) and
# prints the dir. FAKE_MODE=create makes the storage list empty (forces the create
# path); anything else returns an existing, active instance (id 42).
# FAKE_STALE=1 makes the bucket listing include a leftover stage bucket, so the
# provision sweep has something to delete.
# FAKE_BUCKET_DELETE_FAILS=1 makes the control-plane bucket DELETE fail the way a
# non-empty bucket does, for the cleanup paths.
# FAKE_BUCKET_LIST_FAILS=1 makes the bucket LISTING fail too, so cleanup cannot tell
# whether a bucket that refused its delete actually survived.
make_fake_curl() {
  local dir
  dir=$(mktemp -d)
  TMP_DIRS+=("$dir")
  cat > "$dir/curl" <<'FAKE'
#!/usr/bin/env bash
method=GET url=""
prev=""
for a in "$@"; do
  [ "$prev" = "-X" ] && method="$a"
  case "$a" in http://*|https://*) url="$a" ;; esac
  prev="$a"
done
case "$method $url" in
  "DELETE "*/access_keys/*)   echo '{}' ;;
  "POST "*/access_keys)        echo '{"access_key":"NEWKEY","secret_key":"NEWSECRET"}' ;;
  "GET "*/access_keys*)        echo '{"results":[{"access_key":"OLDKEY"}]}' ;;
  "DELETE "*/buckets/*)
    # curl -f exits 22 on an HTTP error, which is how a non-empty bucket surfaces.
    [ "${FAKE_BUCKET_DELETE_FAILS:-}" = "1" ] && exit 22
    echo '{}' ;;
  "POST "*/buckets)            echo '{"name":"created"}' ;;
  "GET "*/buckets*)
    [ "${FAKE_BUCKET_LIST_FAILS:-}" = "1" ] && exit 22
    if [ "${FAKE_STALE:-}" = "1" ]; then
      echo '{"results":[{"name":"lantern-box-stage-deadbeefdeadbeef"},{"name":"keep-me"}]}'
    else
      echo '{"results":[]}'
    fi ;;
  "GET "*/locations*)          echo '{"results":[{"name":"luxembourg-2","technical_name":"s-ed1","type":"s3_compatible"}]}' ;;
  "POST "*/object_storages)    echo '{"id":42,"address":"lux.storage.example","provisioning_status":"active"}' ;;
  "GET "*/object_storages/42)  echo '{"id":42,"address":"lux.storage.example","provisioning_status":"active"}' ;;
  "GET "*/object_storages*)
    if [ "${FAKE_MODE:-existing}" = "create" ]; then
      echo '{"results":[]}'
    else
      echo '{"results":[{"id":42,"name":"lantern-box-images","address":"lux.storage.example","provisioning_status":"active"}]}'
    fi ;;
  *) echo '{}' ;;
esac
exit 0
FAKE
  chmod +x "$dir/curl"
  cat > "$dir/aws" <<'FAKEAWS'
#!/usr/bin/env bash
# Append the invocation to FAKE_AWS_LOG. The sweep redirects aws stdout/stderr to
# /dev/null, so a log file is the only way for a test to observe these calls.
[ -n "${FAKE_AWS_LOG:-}" ] && echo "aws $*" >> "$FAKE_AWS_LOG"
# FAKE_MPU=1 makes list-multipart-uploads report one orphaned upload, which is what
# drives the abort path. Without it the listing is empty, as for a clean bucket.
for a in "$@"; do
  if [ "$a" = list-multipart-uploads ]; then
    [ "${FAKE_MPU:-}" = "1" ] && printf 'stale-key.qcow2\tupload-123\n'
    exit 0
  fi
done
exit 0
FAKEAWS
  chmod +x "$dir/aws"
  echo "$dir"
}

check() { # desc, expected-rc-zero(0/1), output, [grep patterns...]
  local desc="$1" want_ok="$2" out="$3" rc="$4"; shift 4
  local ok=1 pat
  if [ "$want_ok" = "0" ] && [ "$rc" -ne 0 ]; then ok=0; fi
  if [ "$want_ok" = "1" ] && [ "$rc" -eq 0 ]; then ok=0; fi
  for pat in "$@"; do grep -q "$pat" <<<"$out" || ok=0; done
  if [ "$ok" -eq 1 ]; then echo "PASS: $desc"; else echo "FAIL: $desc (rc=$rc)"; echo "$out"; FAILED=1; fi
}

# Test 1: provision against an existing, active instance. The staged bucket is
# randomly named, so assert only the "lantern-box-stage-" prefix.
dir=$(make_fake_curl)
awslog1="$dir/aws.log"
out=$(PATH="$dir:$PATH" FAKE_AWS_LOG="$awslog1" GCORE_API_KEY=k POLL_INTERVAL_SECS=0 bash "$SCRIPT" provision 2>/dev/null) && rc=0 || rc=$?
check "provision (existing instance)" 0 "$out" "$rc" \
  '^storage_id=42$' '^region=s-ed1$' '^endpoint=https://s-ed1.cloud.gcore.lu$' \
  '^bucket=lantern-box-stage-[0-9a-f]\{16\}$' '^access_key=NEWKEY$' '^secret_key=NEWSECRET$'

# Test 1b: the fresh stage bucket gets the 1-day object-expiry rule — the last-resort
# backstop for a run that dies before any teardown runs at all. Gcore implements the
# legacy put-bucket-lifecycle, so assert that spelling specifically.
logged=$(cat "$awslog1" 2>/dev/null || true)
check "provision attaches the 1-day expiry rule" 0 "$logged" 0 \
  's3api put-bucket-lifecycle --bucket lantern-box-stage-[0-9a-f]\{16\}' \
  '"Expiration":{"Days":1}'

# Test 2: provision when the instance must be created.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" FAKE_MODE=create GCORE_API_KEY=k POLL_INTERVAL_SECS=0 bash "$SCRIPT" provision 2>/dev/null) && rc=0 || rc=$?
check "provision (create instance)" 0 "$out" "$rc" '^storage_id=42$' '^region=s-ed1$' \
  '^bucket=lantern-box-stage-[0-9a-f]\{16\}$' '^access_key=NEWKEY$'

# Test 3: provision sweeps a leftover stage bucket (diagnostics go to stderr).
dir=$(make_fake_curl)
awslog="$dir/aws.log"
out=$(PATH="$dir:$PATH" FAKE_STALE=1 FAKE_AWS_LOG="$awslog" GCORE_API_KEY=k POLL_INTERVAL_SECS=0 bash "$SCRIPT" provision 2>&1) && rc=0 || rc=$?
check "provision sweeps stale stage bucket" 0 "$out" "$rc" \
  'sweeping stale stage bucket lantern-box-stage-deadbeefdeadbeef' \
  'revoked anonymous-read policy on lantern-box-stage-deadbeefdeadbeef' \
  'deleted lantern-box-stage-deadbeefdeadbeef'

# Test 3b: the sweep revokes the public policy before emptying the bucket — the
# control plane refuses a non-empty bucket, so without the empty it can't be deleted.
logged=$(cat "$awslog" 2>/dev/null || true)
check "sweep revokes policy then empties the stale bucket" 0 "$logged" 0 \
  's3api delete-bucket-policy --bucket lantern-box-stage-deadbeefdeadbeef' \
  's3 rm s3://lantern-box-stage-deadbeefdeadbeef --recursive'

# Test 3c: the sweep must never touch a bucket outside BUCKET_PREFIX, and must not
# touch the fresh bucket it is about to create for this run.
if grep -q 'keep-me' "$awslog" 2>/dev/null; then
  echo "FAIL: sweep touched the non-stage bucket keep-me"; FAILED=1
else
  echo "PASS: sweep leaves non-stage buckets alone"
fi

# Test 3d: a stale bucket holding an incomplete multipart upload. `s3 rm` does not
# touch those parts and the expiry rule does not cover them, so unless the sweep aborts
# them the control-plane delete can never succeed and the bucket is stranded forever.
dir=$(make_fake_curl)
awslog="$dir/aws.log"
out=$(PATH="$dir:$PATH" FAKE_STALE=1 FAKE_MPU=1 FAKE_AWS_LOG="$awslog" GCORE_API_KEY=k \
  POLL_INTERVAL_SECS=0 bash "$SCRIPT" provision 2>&1) && rc=0 || rc=$?
logged=$(cat "$awslog" 2>/dev/null || true)
check "sweep aborts an orphaned multipart upload" 0 "$logged" "$rc" \
  'list-multipart-uploads --bucket lantern-box-stage-deadbeefdeadbeef' \
  'abort-multipart-upload --bucket lantern-box-stage-deadbeefdeadbeef --key stale-key.qcow2 --upload-id upload-123'

# Test 3e: and it must abort them BEFORE emptying — aborting after the `rm` would leave
# the bucket non-empty at the moment the control-plane delete is attempted.
if awk '/abort-multipart-upload/{a=NR} /s3 rm s3:\/\/lantern-box-stage-deadbeefdeadbeef/{r=NR} END{exit !(a && r && a < r)}' "$awslog"; then
  echo "PASS: multipart abort precedes the object removal"
else
  echo "FAIL: multipart abort ordering"; cat "$awslog"; FAILED=1
fi

# Test 4: cleanup deletes the stage bucket and the access key.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" GCORE_API_KEY=k STORAGE_ID=42 BUCKET=lantern-box-stage-abc ACCESS_KEY=NEWKEY bash "$SCRIPT" cleanup 2>&1) && rc=0 || rc=$?
check "cleanup" 0 "$out" "$rc" 'deleted gcore stage bucket lantern-box-stage-abc' 'deleted gcore access key'

# Test 5: cleanup without BUCKET still deletes the access key.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" GCORE_API_KEY=k STORAGE_ID=42 ACCESS_KEY=NEWKEY bash "$SCRIPT" cleanup 2>&1) && rc=0 || rc=$?
check "cleanup (no bucket)" 0 "$out" "$rc" 'deleted gcore access key'

# Test 5b: a bucket that survives its delete is still holding the qcow2 — cleanup
# must say so and fail rather than quietly stranding it. The access key still goes.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" FAKE_STALE=1 FAKE_BUCKET_DELETE_FAILS=1 GCORE_API_KEY=k \
  STORAGE_ID=42 BUCKET=lantern-box-stage-deadbeefdeadbeef ACCESS_KEY=NEWKEY \
  bash "$SCRIPT" cleanup 2>&1) && rc=0 || rc=$?
check "cleanup fails loudly on a stuck stage bucket" 1 "$out" "$rc" \
  '::error::gcore stage bucket lantern-box-stage-deadbeefdeadbeef' \
  'deleted gcore access key'

# Test 5c: a bucket already gone (delete refused, and it is absent from the listing)
# is not a failure — that is just a race with an earlier sweep.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" FAKE_BUCKET_DELETE_FAILS=1 GCORE_API_KEY=k \
  STORAGE_ID=42 BUCKET=lantern-box-stage-abc ACCESS_KEY=NEWKEY \
  bash "$SCRIPT" cleanup 2>&1) && rc=0 || rc=$?
check "cleanup tolerates an already-gone bucket" 0 "$out" "$rc" \
  'already gone' 'deleted gcore access key'

# Test 5c-2: the delete is refused AND the listing is unreadable, so cleanup cannot tell
# whether the bucket survived. It must assume the worst and fail, never report the
# reassuring "already gone" — folding unknown into absent is how a bucket still holding
# the qcow2 gets signed off as cleaned up.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" FAKE_BUCKET_DELETE_FAILS=1 FAKE_BUCKET_LIST_FAILS=1 GCORE_API_KEY=k \
  STORAGE_ID=42 BUCKET=lantern-box-stage-abc ACCESS_KEY=NEWKEY \
  bash "$SCRIPT" cleanup 2>&1) && rc=0 || rc=$?
check "cleanup fails when it cannot confirm the bucket is gone" 1 "$out" "$rc" \
  'could not be read to confirm' 'deleted gcore access key'
if grep -q 'already gone' <<<"$out"; then
  echo "FAIL: cleanup reported 'already gone' on an unreadable listing"; FAILED=1
fi

# Test 5d: the empty-bucket subcommand is what the workflow's teardown calls. It must do
# the same multipart-then-objects removal as the sweep, since it shares empty_bucket.
dir=$(make_fake_curl)
awslog="$dir/aws.log"
out=$(PATH="$dir:$PATH" FAKE_MPU=1 FAKE_AWS_LOG="$awslog" GCORE_API_KEY=k \
  BUCKET=lantern-box-stage-abc ENDPOINT=https://s-ed1.cloud.gcore.lu \
  bash "$SCRIPT" empty-bucket 2>&1) && rc=0 || rc=$?
logged=$(cat "$awslog" 2>/dev/null || true)
check "empty-bucket aborts multipart uploads and removes objects" 0 "$logged" "$rc" \
  'abort-multipart-upload --bucket lantern-box-stage-abc --key stale-key.qcow2 --upload-id upload-123' \
  's3 rm s3://lantern-box-stage-abc --recursive'
# check() greps each pattern independently, so it cannot see ORDER — assert that
# separately here rather than leaving it implied by the description above.
if awk '/abort-multipart-upload/{a=NR} /s3 rm s3:\/\/lantern-box-stage-abc/{r=NR} END{exit !(a && r && a < r)}' "$awslog"; then
  echo "PASS: empty-bucket aborts before it removes"
else
  echo "FAIL: empty-bucket ordering"; cat "$awslog"; FAILED=1
fi

# Test 5e: a clean bucket has no multipart uploads to abort, and that is not an error.
dir=$(make_fake_curl)
awslog="$dir/aws.log"
out=$(PATH="$dir:$PATH" FAKE_AWS_LOG="$awslog" GCORE_API_KEY=k \
  BUCKET=lantern-box-stage-abc ENDPOINT=https://s-ed1.cloud.gcore.lu \
  bash "$SCRIPT" empty-bucket 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] && ! grep -q 'abort-multipart-upload' "$awslog"; then
  echo "PASS: empty-bucket skips the abort when there are no orphaned uploads"
else
  echo "FAIL: empty-bucket on a clean bucket (rc=$rc)"; cat "$awslog"; FAILED=1
fi

# Test 5f: empty-bucket needs BUCKET and ENDPOINT, and says so rather than acting on an
# empty bucket name.
out=$(GCORE_API_KEY=k bash "$SCRIPT" empty-bucket 2>&1) && rc=0 || rc=$?
check "empty-bucket requires BUCKET" 1 "$out" "$rc" 'BUCKET must be set'

# Test 6: missing GCORE_API_KEY fails fast.
out=$(bash "$SCRIPT" provision 2>&1) && rc=0 || rc=$?
check "missing GCORE_API_KEY" 1 "$out" "$rc" 'must be set'

# Test 7: unknown subcommand is a usage error.
out=$(GCORE_API_KEY=k bash "$SCRIPT" bogus 2>&1) && rc=0 || rc=$?
check "unknown subcommand" 1 "$out" "$rc" 'usage:'

if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
cleanup_tmp # explicit (the EXIT trap is the safety net for early exits); rm -rf is idempotent
exit "$FAILED"
