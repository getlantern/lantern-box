#!/usr/bin/env bash
# Tests for gcore-storage.sh using a fake `curl` on PATH (no network). The fake
# emulates the gcore object-storage control plane: list/create storages, get a
# storage, list/create buckets, and list/create/delete access keys.
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

# Writes a fake `curl` to a fresh temp dir (prepended to PATH) and prints the dir.
# FAKE_MODE=create makes the storage list empty (forces the create path);
# anything else returns an existing, active instance (id 42).
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
  "POST "*/buckets)            echo '{"name":"lantern-box-images"}' ;;
  "GET "*/buckets*)            echo '{"results":[]}' ;;
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

# Test 1: provision against an existing, active instance.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" GCORE_API_KEY=k POLL_INTERVAL_SECS=0 bash "$SCRIPT" provision 2>/dev/null) && rc=0 || rc=$?
check "provision (existing instance)" 0 "$out" "$rc" \
  '^storage_id=42$' '^region=s-ed1$' '^endpoint=https://s-ed1.cloud.gcore.lu$' \
  '^bucket=lantern-box-images$' '^access_key=NEWKEY$' '^secret_key=NEWSECRET$'

# Test 2: provision when the instance must be created.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" FAKE_MODE=create GCORE_API_KEY=k POLL_INTERVAL_SECS=0 bash "$SCRIPT" provision 2>/dev/null) && rc=0 || rc=$?
check "provision (create instance)" 0 "$out" "$rc" '^storage_id=42$' '^region=s-ed1$' '^access_key=NEWKEY$'

# Test 3: cleanup deletes the access key.
dir=$(make_fake_curl)
out=$(PATH="$dir:$PATH" GCORE_API_KEY=k STORAGE_ID=42 ACCESS_KEY=NEWKEY bash "$SCRIPT" cleanup 2>&1) && rc=0 || rc=$?
check "cleanup" 0 "$out" "$rc" 'deleted gcore access key'

# Test 4: missing GCORE_API_KEY fails fast.
out=$(bash "$SCRIPT" provision 2>&1) && rc=0 || rc=$?
check "missing GCORE_API_KEY" 1 "$out" "$rc" 'must be set'

# Test 5: unknown subcommand is a usage error.
out=$(GCORE_API_KEY=k bash "$SCRIPT" bogus 2>&1) && rc=0 || rc=$?
check "unknown subcommand" 1 "$out" "$rc" 'usage:'

if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; fi
cleanup_tmp # explicit (the EXIT trap is the safety net for early exits); rm -rf is idempotent
exit "$FAILED"
