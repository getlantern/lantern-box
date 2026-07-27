#!/usr/bin/env bash
# Tests for gcore-publish.sh. Puts a fake `curl` on PATH so no network is used.
# Run: bash deploy/packer/gcore-publish.test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBLISH="${SCRIPT_DIR}/gcore-publish.sh"
FAILED=0

# Every make_fake_curl call's temp dir is recorded here and removed on exit so
# no stray dirs are left behind. This runs after $FAILED is already decided by
# the final `exit "$FAILED"`, so it never masks a test failure or changes the
# script's exit code.
TMP_DIRS=()
cleanup() {
  local d
  for d in "${TMP_DIRS[@]:-}"; do
    [ -n "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

# Writes a fake `curl` into a fresh temp dir and prints the dir. Runs via
# command substitution, i.e. in a subshell, so it cannot append to TMP_DIRS
# itself; every call site below does `dir=$(make_fake_curl); TMP_DIRS+=("$dir")`
# so the dir is still tracked for cleanup. The fake curl ignores flags and
# responds based on the request URL and FAKE_CURL_MODE (default "ok"):
#   ok             downloadimage -> task "task-abc"; /tasks/ -> FINISHED
#   import_error   /tasks/ -> ERROR
#   retry          /tasks/ -> RUNNING until a per-invocation counter file
#                  (next to the fake curl) reaches FAKE_CURL_RETRY_THRESHOLD
#                  (default 2), then FINISHED
#   always_running /tasks/ -> RUNNING, always (for the timeout test)
#   mixed          downloadimage -> task named after the region in the URL
#                  (e.g. region 68 -> "task-68"); /tasks/ -> ERROR for
#                  FAKE_CURL_ERROR_REGION's task, FINISHED for every other
#                  region's task
make_fake_curl() {
  local dir
  dir=$(mktemp -d)
  cat > "$dir/curl" <<'FAKE'
#!/usr/bin/env bash
self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mode="${FAKE_CURL_MODE:-ok}"
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
            echo '{"state":"FINISHED"}'
          else
            echo $((count + 1)) > "$count_file"
            echo '{"state":"RUNNING"}'
          fi ;;
        mixed)
          region="${task#task-}"
          if [ "$region" = "${FAKE_CURL_ERROR_REGION:-}" ]; then
            echo '{"state":"ERROR"}'
          else
            echo '{"state":"FINISHED"}'
          fi ;;
        *)
          echo '{"state":"FINISHED"}' ;;
      esac
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
   && grep -q "region 68: import task task-abc FINISHED" <<<"$out"; then
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

# Test 4: poll retries on a non-terminal state, then succeeds -> the fake
# curl returns RUNNING for the first two /tasks/ polls and FINISHED after,
# exercising poll_task's sleep-and-retry branch; script still exits 0.
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

# Test 5: poll timeout -> the fake curl always returns RUNNING, so
# poll_task exhausts POLL_ATTEMPTS and the script exits non-zero with the
# "did not finish after" hard-failure message (no fallback).
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

# Test 6: mixed multi-region outcome -> region 180 FINISHED, region 68
# ERROR; the failures counter aggregates across regions and the script
# exits non-zero even though one region succeeded.
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

# Test 7: downloadimage returns an HTTP error -> non-zero exit, status + body
# surfaced (rather than the empty message the old `curl -f` produced).
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

if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
# Explicit call (the EXIT trap above is a safety net for any other exit path);
# cleanup is idempotent, and neither invocation touches $FAILED or calls exit,
# so this cannot mask a test failure or change the exit code below.
cleanup
exit "$FAILED"
