#!/usr/bin/env bash
# Tests for gcore-publish.sh. Puts a fake `curl` on PATH so no network is used.
# Run: bash deploy/packer/gcore-publish.test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBLISH="${SCRIPT_DIR}/gcore-publish.sh"
FAILED=0

# Writes a fake `curl` into a fresh temp dir and prints the dir. The fake curl
# ignores flags and responds based on the request URL: a downloadimage POST
# returns a task id; a /tasks/ GET returns FINISHED (or ERROR when
# FAKE_CURL_MODE=import_error).
make_fake_curl() {
  local dir
  dir=$(mktemp -d)
  cat > "$dir/curl" <<'FAKE'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    *downloadimage*) echo '{"tasks":["task-abc"]}'; exit 0 ;;
    */tasks/*)
      if [ "${FAKE_CURL_MODE:-ok}" = "import_error" ]; then
        echo '{"state":"ERROR"}'
      else
        echo '{"state":"FINISHED"}'
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
dir=$(make_fake_curl)
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
dir=$(make_fake_curl)
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

if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
exit "$FAILED"
