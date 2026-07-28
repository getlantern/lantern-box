#!/usr/bin/env bash
# Tests for gcore-publish.sh. Puts a fake `curl` on PATH so no network is used.
# Run: bash deploy/packer/gcore-publish.test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBLISH="${SCRIPT_DIR}/gcore-publish.sh"
FAILED=0

# Temp dirs to remove on exit. Runs after $FAILED is already decided, so it can
# never mask a failure or change the exit code.
TMP_DIRS=()
cleanup() {
  local d
  for d in "${TMP_DIRS[@]:-}"; do
    [ -n "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

# Writes a fake `curl` into a fresh temp dir and prints the dir. It runs in a
# subshell, so callers do `dir=$(make_fake_curl); TMP_DIRS+=("$dir")` to track it.
# The fake ignores flags and answers on URL plus FAKE_CURL_MODE (default "ok"):
#   ok             downloadimage -> task "task-abc"; /tasks/ -> FINISHED
#   import_error   /tasks/ -> ERROR
#   retry          /tasks/ -> RUNNING until a counter file next to the fake curl
#                  reaches FAKE_CURL_RETRY_THRESHOLD (default 2), then FINISHED
#   always_running /tasks/ -> RUNNING, always (for the timeout test)
#   mixed          downloadimage -> task named for the region in the URL (region
#                  68 -> "task-68"); /tasks/ -> ERROR only for
#                  FAKE_CURL_ERROR_REGION's task
# FINISHED tasks report created_resources.images == ["img-1"], and /images/...
# returns FAKE_CURL_VISIBILITY (default "private") for report_visibility.
make_fake_curl() {
  local dir
  dir=$(mktemp -d)
  cat > "$dir/curl" <<'FAKE'
#!/usr/bin/env bash
self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mode="${FAKE_CURL_MODE:-ok}"
finished='{"state":"FINISHED","created_resources":{"images":["img-1"]}}'
for a in "$@"; do
  case "$a" in
    *downloadimage*)
      # import_region uses `curl -w '\n%{http_code}'`, so append a status line.
      http="${FAKE_CURL_IMPORT_HTTP:-200}"
      if [ "$http" != 200 ]; then
        printf '%s\n%s' '{"detail":"import rejected"}' "$http"
      elif [ "$mode" = mixed ]; then
        region="${a##*/}"
        printf '%s\n%s' "{\"tasks\":[\"task-${region}\"]}" 200
      else
        printf '%s\n%s' '{"tasks":["task-abc"]}' 200
      fi
      exit 0 ;;
    */tasks/*)
      task="${a##*/tasks/}"
      case "$mode" in
        import_error)
          echo '{"state":"ERROR"}' ;;
        always_running)
          echo '{"state":"RUNNING"}' ;;
        retry)
          count_file="${self_dir}/poll_count"
          [ -f "$count_file" ] || echo 0 > "$count_file"
          count=$(cat "$count_file")
          if [ "$count" -ge "${FAKE_CURL_RETRY_THRESHOLD:-2}" ]; then
            echo "$finished"
          else
            echo $((count + 1)) > "$count_file"
            echo '{"state":"RUNNING"}'
          fi ;;
        mixed)
          region="${task#task-}"
          if [ "$region" = "${FAKE_CURL_ERROR_REGION:-}" ]; then
            echo '{"state":"ERROR"}'
          else
            echo "$finished"
          fi ;;
        *)
          echo "$finished" ;;
      esac
      exit 0 ;;
    */images/*)
      # report_visibility's GET /images/<project>/<region>/<image_id>. Deliberately
      # does not match the downloadimage URL ("downloadimage/" is not "/images/").
      if [ -n "${FAKE_CURL_VISIBILITY_MISSING:-}" ]; then
        echo '{}'
      else
        printf '{"visibility":"%s"}\n' "${FAKE_CURL_VISIBILITY:-private}"
      fi
      exit 0 ;;
  esac
done
echo '{}'; exit 0
FAKE
  chmod +x "$dir/curl"
  echo "$dir"
}

# Test 1: happy path, two regions -> exit 0, both FINISHED.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180,68" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] \
   && grep -q "region 180: import task task-abc FINISHED" <<<"$out" \
   && grep -q "region 68: import task task-abc FINISHED" <<<"$out" \
   && grep -q "region 180: image img-1 visibility=private" <<<"$out" \
   && grep -q "region 68: image img-1 visibility=private" <<<"$out"; then
  echo "PASS: happy path (two regions)"
else
  echo "FAIL: happy path (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 2: import task ERROR -> non-zero exit, error surfaced.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_MODE=import_error \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] && grep -q "state ERROR" <<<"$out"; then
  echo "PASS: import error path"
else
  echo "FAIL: import error path (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 3: missing required env -> non-zero exit with a clear message.
out=$(GCORE_API_KEY=k bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] && grep -q "must be set" <<<"$out"; then
  echo "PASS: missing env path"
else
  echo "FAIL: missing env path (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 4: RUNNING for the first two polls, then FINISHED -> exercises poll_task's
# sleep-and-retry branch; still exits 0.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_MODE=retry FAKE_CURL_RETRY_THRESHOLD=2 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
running_count=$(grep -c "state=RUNNING" <<<"$out")
if [ "$rc" -eq 0 ] && [ "$running_count" -eq 2 ] \
   && grep -q "region 180: import task task-abc FINISHED" <<<"$out"; then
  echo "PASS: poll retry then success"
else
  echo "FAIL: poll retry then success (rc=$rc, running_count=$running_count)"; echo "$out"; FAILED=1
fi

# Test 5: always RUNNING -> poll_task exhausts POLL_ATTEMPTS and hard-fails (no
# fallback).
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_MODE=always_running \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_ATTEMPTS=2 POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] && grep -q "did not finish after 2 attempts" <<<"$out"; then
  echo "PASS: poll timeout"
else
  echo "FAIL: poll timeout (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 6: region 180 FINISHED, region 68 ERROR -> failures aggregate across
# regions, so the script exits non-zero even though one region succeeded.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_MODE=mixed FAKE_CURL_ERROR_REGION=68 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180,68" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "region 180: import task task-180 FINISHED" <<<"$out" \
   && grep -q "region 68: import task task-68 state ERROR" <<<"$out"; then
  echo "PASS: mixed multi-region outcome"
else
  echo "FAIL: mixed multi-region outcome (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 7: downloadimage HTTP error -> non-zero exit with status + body surfaced,
# not the empty message `curl -f` used to produce.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_IMPORT_HTTP=422 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] && grep -q "HTTP 422" <<<"$out" && grep -q "import rejected" <<<"$out"; then
  echo "PASS: import HTTP error surfaces status + body"
else
  echo "FAIL: import HTTP error (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 8: image comes back public -> import succeeded, but exit non-zero, since
# public means gcore's global catalog rather than private to the project.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY=public \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "import task task-abc FINISHED" <<<"$out" \
   && grep -q "visibility=public" <<<"$out"; then
  echo "PASS: public image fails the publish"
else
  echo "FAIL: public image fails the publish (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 9: image comes back "shared" -> owner-only, so a warning rather than a
# failure, because ?private=true listings won't return it.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY=shared \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] \
   && grep -q "visibility=shared, not private" <<<"$out" \
   && grep -q "will NOT return this image" <<<"$out"; then
  echo "PASS: shared image warns but succeeds"
else
  echo "FAIL: shared image warns but succeeds (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 10: unreadable visibility -> warn and succeed; the import already finished,
# so a failed diagnostic must not fail the publish.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY_MISSING=1 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] && grep -q "could not read visibility of image img-1" <<<"$out"; then
  echo "PASS: unreadable visibility warns but succeeds"
else
  echo "FAIL: unreadable visibility warns but succeeds (rc=$rc)"; echo "$out"; FAILED=1
fi

if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
# The EXIT trap covers other exit paths; cleanup is idempotent and touches neither
# $FAILED nor exit, so calling it twice is harmless.
cleanup
exit "$FAILED"
