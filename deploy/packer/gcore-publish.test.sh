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
# FINISHED tasks report created_resources.images == ["img-1"] — unless
# FAKE_CURL_NO_IMAGE_ID is set, which empties the list so report_visibility cannot
# resolve the image at all. GET /images/... returns FAKE_CURL_VISIBILITY (default
# "private") for report_visibility; FAKE_CURL_VISIBILITY_MISSING makes every such GET
# unreadable, and FAKE_CURL_VISIBILITY_FLAKY=N makes only the first N unreadable so a
# test can prove the bounded retry recovers.
# DELETE /images/... (delete_image) answers FAKE_CURL_DELETE_HTTP (default 204) and
# returns a "task-del" task, whose state comes from FAKE_CURL_DELETE_STATE
# (default FINISHED).
make_fake_curl() {
  local dir
  dir=$(mktemp -d)
  cat > "$dir/curl" <<'FAKE'
#!/usr/bin/env bash
self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mode="${FAKE_CURL_MODE:-ok}"
finished='{"state":"FINISHED","created_resources":{"images":["img-1"]}}'
if [ -n "${FAKE_CURL_NO_IMAGE_ID:-}" ]; then
  finished='{"state":"FINISHED","created_resources":{"images":[]}}'
fi
# The image DELETE and the visibility GET share a URL shape, so the method has to be
# read off the flags before any URL matching.
method=GET prev=""
for a in "$@"; do
  [ "$prev" = "-X" ] && method="$a"
  prev="$a"
done
if [ "$method" = DELETE ]; then
  for a in "$@"; do
    case "$a" in
      */images/*)
        http="${FAKE_CURL_DELETE_HTTP:-204}"
        if [ "$http" != 204 ]; then
          printf '%s\n%s' '{"detail":"delete refused"}' "$http"
        else
          printf '%s\n%s' '{"tasks":["task-del"]}' "$http"
        fi
        exit 0 ;;
    esac
  done
fi
for a in "$@"; do
  # A delete task must resolve before the generic /tasks/ handling below, which is
  # driven by the import-oriented modes.
  case "$a" in
    */tasks/task-del)
      printf '{"state":"%s"}\n' "${FAKE_CURL_DELETE_STATE:-FINISHED}"
      exit 0 ;;
  esac
done
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
      if [ -n "${FAKE_CURL_VISIBILITY_FLAKY:-}" ]; then
        # Unreadable for the first N calls, then real: proves fetch_field's retry turns
        # a transient blip back into a pass rather than failing the publish.
        vcount_file="${self_dir}/vis_count"
        [ -f "$vcount_file" ] || echo 0 > "$vcount_file"
        vcount=$(cat "$vcount_file")
        echo $((vcount + 1)) > "$vcount_file"
        if [ "$vcount" -lt "$FAKE_CURL_VISIBILITY_FLAKY" ]; then
          echo '{}'; exit 0
        fi
      fi
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

# Test 8: image comes back public -> the exposure is remediated by deleting it, and
# the publish still fails, because the region ends up with no usable private image.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY=public \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "import task task-abc FINISHED" <<<"$out" \
   && grep -q "visibility=public" <<<"$out" \
   && grep -q "region 180: delete task task-del FINISHED" <<<"$out" \
   && grep -q "public image img-1 removed" <<<"$out"; then
  echo "PASS: public image is deleted and fails the publish"
else
  echo "FAIL: public image is deleted and fails the publish (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 8b: public image whose delete is refused -> say plainly that it is still in
# the public catalog and needs a human, rather than implying it was cleaned up.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY=public FAKE_CURL_DELETE_HTTP=409 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "HTTP 409" <<<"$out" && grep -q "delete refused" <<<"$out" \
   && grep -q "could NOT be deleted" <<<"$out"; then
  echo "PASS: refused delete of a public image is reported as still public"
else
  echo "FAIL: refused delete of a public image (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 8c: delete accepted but its task ends in ERROR -> also "could NOT be deleted".
# An accepted-then-failed delete leaves the image public just as a refusal does.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY=public FAKE_CURL_DELETE_STATE=ERROR \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "delete task task-del state ERROR" <<<"$out" \
   && grep -q "could NOT be deleted" <<<"$out"; then
  echo "PASS: failed delete task of a public image is reported as still public"
else
  echo "FAIL: failed delete task of a public image (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 9: image comes back "shared" -> project-only in practice, so a warning rather
# than a failure. It must NOT be deleted: it is usable and not an exposure.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY=shared \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] \
   && grep -q "visibility=shared, not private" <<<"$out" \
   && grep -q "prune still collects it" <<<"$out" \
   && ! grep -q "task-del" <<<"$out"; then
  echo "PASS: shared image warns, is not deleted, and succeeds"
else
  echo "FAIL: shared image warns but succeeds (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 10: unreadable visibility -> FAIL the publish once the retries are exhausted.
# This is the check that catches an image landing in gcore's public catalog, so "we
# could not tell" must not read as "it is fine". Retries are bounded, so the failure
# arrives after VISIBILITY_ATTEMPTS tries rather than hanging.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY_MISSING=1 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 VISIBILITY_ATTEMPTS=2 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "could not read visibility of image img-1 after 2 attempts" <<<"$out" \
   && grep -q "treat it as exposed" <<<"$out" \
   && grep -q "visibility of image img-1 lookup failed (attempt 1/2)" <<<"$out" \
   && grep -q "1 gcore region import(s) failed" <<<"$out"; then
  echo "PASS: unverifiable visibility fails the publish after bounded retries"
else
  echo "FAIL: unverifiable visibility fails the publish (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 10b: a visibility lookup that fails once and then succeeds must still pass. The
# fail-closed behaviour above is only acceptable because one API blip does not sink an
# otherwise good build.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_VISIBILITY_FLAKY=1 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] \
   && grep -q "visibility of image img-1 lookup failed (attempt 1/3); retrying" <<<"$out" \
   && grep -q "region 180: image img-1 visibility=private" <<<"$out"; then
  echo "PASS: a flaky visibility lookup recovers on retry"
else
  echo "FAIL: a flaky visibility lookup recovers on retry (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 10c: the other half of the check — the task exposes no image ID, so visibility
# cannot even be looked up. Same reasoning as test 10: fail rather than skip.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_NO_IMAGE_ID=1 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180" POLL_INTERVAL_SECS=0 VISIBILITY_ATTEMPTS=2 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "could not resolve the imported image ID from task task-abc after 2 attempts" <<<"$out" \
   && grep -q "lantern-box-0.0.0" <<<"$out" \
   && grep -q "1 gcore region import(s) failed" <<<"$out"; then
  echo "PASS: an unresolvable image ID fails the publish"
else
  echo "FAIL: an unresolvable image ID fails the publish (rc=$rc)"; echo "$out"; FAILED=1
fi

# Test 11: every region's import is issued BEFORE any polling starts, and the wait costs
# one sleep per round rather than one per region per round. That is the property making
# wall-clock the slowest single region instead of the sum: draining regions one at a time
# would cost regions x POLL_ATTEMPTS sleeps (6 here) and would not POST the later imports
# until the earlier ones finished.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
sleeplog="$dir/sleeps"
printf '#!/usr/bin/env bash\necho s >> "%s"\n' "$sleeplog" > "$dir/fakesleep"
chmod +x "$dir/fakesleep"
out=$(PATH="$dir:$PATH" FAKE_CURL_MODE=always_running SLEEP="$dir/fakesleep" \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180,68,45" POLL_ATTEMPTS=2 POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
sleeps=$(grep -c . "$sleeplog" 2>/dev/null || echo 0)
# sed rather than head, which would SIGPIPE the grep feeding it under pipefail.
last_import=$(grep -n '==> Importing' <<<"$out" | sed -n '$p' | cut -d: -f1)
first_poll=$(grep -n 'state=RUNNING' <<<"$out" | sed -n '1p' | cut -d: -f1)
timeouts=$(grep -c 'did not finish after 2 attempts' <<<"$out")
if [ "$rc" -ne 0 ] && [ "$timeouts" -eq 3 ] && [ "$sleeps" -le 2 ] \
   && [ -n "$last_import" ] && [ -n "$first_poll" ] && [ "$last_import" -lt "$first_poll" ]; then
  echo "PASS: imports all start before polling, ${sleeps} sleep(s) for 3 regions x 2 attempts"
else
  echo "FAIL: concurrent import/poll (rc=$rc timeouts=$timeouts sleeps=$sleeps import@$last_import poll@$first_poll)"
  echo "$out"; FAILED=1
fi

# Test 12: a region whose import POST is rejected must not stop the others from being
# started or waited on — the failure is counted and the rest still publish.
dir=$(make_fake_curl); TMP_DIRS+=("$dir")
out=$(PATH="$dir:$PATH" FAKE_CURL_MODE=mixed FAKE_CURL_ERROR_REGION=68 \
  GCORE_API_KEY=k GCORE_PROJECT_ID=1 VERSION=0.0.0 IMAGE_URL=http://example/x.qcow2 \
  GCORE_REGIONS="180,68,45" POLL_INTERVAL_SECS=0 \
  bash "$PUBLISH" 2>&1) && rc=0 || rc=$?
if [ "$rc" -ne 0 ] \
   && grep -q "region 180: import task task-180 FINISHED" <<<"$out" \
   && grep -q "region 45: import task task-45 FINISHED" <<<"$out" \
   && grep -q "region 68: import task task-68 state ERROR" <<<"$out" \
   && grep -q "1 gcore region import(s) failed" <<<"$out"; then
  echo "PASS: one failing region does not block the others"
else
  echo "FAIL: one failing region does not block the others (rc=$rc)"; echo "$out"; FAILED=1
fi

if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
# The EXIT trap covers other exit paths; cleanup is idempotent and touches neither
# $FAILED nor exit, so calling it twice is harmless.
cleanup
exit "$FAILED"
