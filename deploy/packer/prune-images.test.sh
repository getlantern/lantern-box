#!/usr/bin/env bash
# Tests for prune-images.sh. Puts fake `oci` and `aliyun` CLIs on PATH, so no
# network is used. Each fake answers from JSON files in its fixture dir and
# appends every call to calls.log there.
# Run: bash deploy/packer/prune-images.test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PRUNE="${SCRIPT_DIR}/prune-images.sh"
FIXTURES="${SCRIPT_DIR}/testdata/prune-images"
FAILED=0

export SNAPSHOT_RETRY_SECS=0 ALICLOUD_RETRY_SECS=0

TMP_DIRS=()
cleanup() {
  local d
  for d in "${TMP_DIRS[@]:-}"; do
    [ -n "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

# Copies the fakes and their fixtures into a fresh dir and prints it.
make_env() {
  local dir
  dir=$(mktemp -d)
  cp "$FIXTURES"/* "$dir/"
  chmod +x "$dir/oci" "$dir/aliyun"
  : >"$dir/calls.log"
  echo "$dir"
}

pass() { echo "PASS: $1"; }
failt() {
  echo "FAIL: $1"
  FAILED=1
}

# Runs the script with the fakes first on PATH; output goes to $dir/out.
run_prune() {
  local dir=$1
  shift
  env PATH="$dir:$PATH" FAKE_DIR="$dir" \
    OCI_CLI_CONFIG_FILE=/dev/null OCI_TENANCY_OCID=ocid1.tenancy.t OCI_COMPARTMENT_OCID=ocid1.compartment.ours \
    ALICLOUD_ACCESS_KEY=ak ALICLOUD_SECRET_KEY=sk \
    "$@" >"$dir/out" 2>&1
}

deleted_oci() { grep -o 'compute image delete --region [^ ]* --image-id [^ ]*' "$1/calls.log" | awk '{print $5":"$7}' | sort | tr '\n' ' '; }
deleted_ali_images() { grep -o 'ecs DeleteImage --RegionId [^ ]* --ImageId [^ ]*' "$1/calls.log" | awk '{print $4":"$6}' | sort | tr '\n' ' '; }
deleted_snapshots() { grep -o 'ecs DeleteSnapshot --region [^ ]* --SnapshotId [^ ]*' "$1/calls.log" | awk '{print $6}' | sort -u | tr '\n' ' '; }

# 1. OCI keeps the newest KEEP per region and deletes the rest; images outside
#    the compartment or without the prefix are never touched.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=2 "$PRUNE" oci; then
  got=$(deleted_oci "$dir")
  want="r1:img-r1-1 r1:img-r1-2 "
  if [ "$got" = "$want" ]; then pass "oci prunes past KEEP per region"; else failt "oci deleted [$got], want [$want]"; fi
else
  failt "oci exited non-zero: $(cat "$dir/out")"
fi

# 2. OCI dry run deletes nothing.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=2 DRY_RUN=1 "$PRUNE" oci && [ -z "$(deleted_oci "$dir")" ] \
  && grep -q "would delete lantern-box-1-arm64-r1" "$dir/out"; then
  pass "oci dry run deletes nothing"
else
  failt "oci dry run: $(cat "$dir/out")"
fi

# 3. A region that cannot be listed fails the run, but the others are still pruned.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=2 FAKE_OCI_FAIL_REGION=r2 "$PRUNE" oci; then
  failt "oci exited zero with an unlistable region"
elif [ "$(deleted_oci "$dir")" = "r1:img-r1-1 r1:img-r1-2 " ] && grep -q "OCI r2: cannot list images" "$dir/out"; then
  pass "oci unlistable region fails the run, others still pruned"
else
  failt "oci unlistable region: $(cat "$dir/out")"
fi

# 4. Alicloud deletes the old images and their snapshots, skips one an instance
#    uses, and finishes the snapshot of an image an earlier run deleted; the
#    snapshot of an image that still exists stays.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 "$PRUNE" alicloud; then
  got=$(deleted_ali_images "$dir")
  snaps=$(deleted_snapshots "$dir")
  if [ "$got" = "ap-southeast-1:m-old1 " ] && [ "$snaps" = "s-leftover s-old1 " ] \
    && grep -q "skipping lantern-box-2 (m-used)" "$dir/out" \
    && grep -q "ResourceId.1 s-old1 .*Tag.1.Value m-old1" "$dir/calls.log"; then
    pass "alicloud prunes images and snapshots, skips in-use"
  else
    failt "alicloud deleted [$got] snapshots [$snaps]: $(cat "$dir/out")"
  fi
else
  failt "alicloud exited non-zero: $(cat "$dir/out")"
fi

# 5. A region that rejects the account (InvalidRegionId) is skipped, not a failure.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 "$PRUNE" alicloud && grep -q "cn-nanjing: region not open to this account" "$dir/out"; then
  pass "alicloud InvalidRegionId region skipped"
else
  failt "alicloud InvalidRegionId: $(cat "$dir/out")"
fi

# 6. A snapshot that never deletes fails the run.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 FAKE_ALI_SNAPSHOT_FAIL=1 "$PRUNE" alicloud; then
  failt "alicloud exited zero with an undeletable snapshot"
elif grep -q "failed to delete snapshot s-old1" "$dir/out"; then
  pass "alicloud undeletable snapshot fails the run"
else
  failt "alicloud undeletable snapshot: $(cat "$dir/out")"
fi

# 7. When an image's snapshots cannot be tagged, the image is kept, so its
#    snapshots stay findable.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 FAKE_ALI_TAG_FAIL=1 "$PRUNE" alicloud; then
  failt "alicloud exited zero with untaggable snapshots"
elif [ -z "$(deleted_ali_images "$dir")" ] && grep -q "cannot tag the snapshots of lantern-box-1" "$dir/out"; then
  pass "alicloud untaggable snapshots keep the image"
else
  failt "alicloud untaggable snapshots: $(cat "$dir/out")"
fi

# 8. A failed DeleteImage whose follow-up lookup also fails counts as a failure.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 FAKE_ALI_DELETE_IMAGE_FAIL=1 "$PRUNE" alicloud; then
  failt "alicloud exited zero with an unconfirmed image delete"
elif grep -q "failed to delete lantern-box-1 (m-old1)" "$dir/out" && ! grep -q "deleted snapshot s-old1" "$dir/out"; then
  pass "alicloud unconfirmed image delete fails"
else
  failt "alicloud unconfirmed image delete: $(cat "$dir/out")"
fi

# 9. A tagged snapshot whose tag value is not an image ID is never deleted.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 FAKE_ALI_BADTAG=1 "$PRUNE" alicloud; then
  failt "alicloud exited zero with a malformed snapshot tag"
elif ! grep -q "DeleteSnapshot .*s-badtag" "$dir/calls.log" && grep -q "s-badtag carries" "$dir/out"; then
  pass "alicloud malformed snapshot tag left alone"
else
  failt "alicloud malformed snapshot tag: $(cat "$dir/out")"
fi

# 10. A transient Alicloud error is retried.
dir=$(make_env)
TMP_DIRS+=("$dir")
if run_prune "$dir" KEEP=1 FAKE_ALI_FLAKY_ONCE=1 "$PRUNE" alicloud && [ "$(deleted_ali_images "$dir")" = "ap-southeast-1:m-old1 " ]; then
  pass "alicloud transient error retried"
else
  failt "alicloud transient error: $(cat "$dir/out")"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "Some tests failed"
  exit 1
fi
echo "All tests passed"
