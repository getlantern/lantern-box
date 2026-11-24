#!/usr/bin/env bash
# Deletes all but the newest KEEP lantern-box images in every region of an OCI
# tenancy or Alicloud account. Regions come from the provider, not from the
# packer config, so a region the build no longer publishes to is still pruned.
#
# lantern-cloud provisions from the newest image in each region, so the older
# ones are only rollback room.
#
# On Alicloud the snapshots backing a deleted image are deleted too. They are
# tagged with the image ID before the image goes, and every run deletes the
# tagged snapshots whose image is gone, so a cleanup that failed or was
# interrupted is finished by the next run.
#
# Usage: prune-images.sh oci|alicloud
#
# Common env:
#   KEEP     images to keep per region (default 3)
#   PREFIX   image name prefix (default lantern-box-)
#   DRY_RUN  when 1, print what would be deleted and delete nothing
# oci env (an OCI CLI config file with a DEFAULT profile):
#   OCI_CLI_CONFIG_FILE, OCI_TENANCY_OCID, OCI_COMPARTMENT_OCID
# alicloud env:
#   ALICLOUD_ACCESS_KEY, ALICLOUD_SECRET_KEY
#   ALICLOUD_HOME_REGION  region used to list the others (default ap-southeast-1)
#
# Exits non-zero when any region could not be listed or any delete failed; the
# other regions are still processed.
set -euo pipefail

provider=${1:-}
KEEP=${KEEP:-3}
PREFIX=${PREFIX:-lantern-box-}
DRY_RUN=${DRY_RUN:-0}
ALICLOUD_HOME_REGION=${ALICLOUD_HOME_REGION:-ap-southeast-1}
# Seconds between DeleteSnapshot attempts while Alicloud releases the image's
# hold on its snapshot.
SNAPSHOT_RETRY_SECS=${SNAPSHOT_RETRY_SECS:-10}
ALICLOUD_RETRY_SECS=${ALICLOUD_RETRY_SECS:-5}

failures=0
deleted=0
errfile=$(mktemp)
trap 'rm -f "$errfile"' EXIT

fail() {
  echo "::error::$*"
  failures=$((failures + 1))
}

# Prints the entries past the newest KEEP. Input: one "<created>\t<id>\t<name>"
# line per image, any order.
excess() {
  sort -r | tail -n +$((KEEP + 1))
}

# --- OCI -------------------------------------------------------------------

oci_cli() {
  oci --config-file "$OCI_CLI_CONFIG_FILE" "$@"
}

prune_oci_region() {
  local region=$1 listing images created id name
  if ! listing=$(oci_cli compute image list --region "$region" --compartment-id "$OCI_COMPARTMENT_OCID" \
    --lifecycle-state AVAILABLE --all); then
    fail "OCI $region: cannot list images"
    return
  fi
  # The CLI prints nothing at all for an empty listing.
  [ -n "$listing" ] || listing='{}'
  # The listing also carries platform images, which belong to no compartment of ours.
  images=$(jq -r --arg c "$OCI_COMPARTMENT_OCID" --arg p "$PREFIX" \
    '.data[]? | select(."compartment-id" == $c and (."display-name" | startswith($p)))
     | "\(."time-created")\t\(.id)\t\(."display-name")"' <<<"$listing")
  echo "OCI $region: $(grep -c . <<<"$images" || true) ${PREFIX}* images (keeping $KEEP)"
  while IFS=$'\t' read -r created id name; do
    [ -n "$id" ] || continue
    if [ "$DRY_RUN" = "1" ]; then
      echo "  would delete $name ($id, created $created)"
      continue
    fi
    echo "  deleting $name ($id, created $created)"
    if oci_cli compute image delete --region "$region" --image-id "$id" --force >/dev/null; then
      deleted=$((deleted + 1))
    else
      fail "OCI $region: failed to delete $name ($id)"
    fi
  done < <(excess <<<"$images")
}

prune_oci() {
  local regions region
  : "${OCI_CLI_CONFIG_FILE:?}" "${OCI_TENANCY_OCID:?}" "${OCI_COMPARTMENT_OCID:?}"
  if ! regions=$(oci_cli iam region-subscription list --tenancy-id "$OCI_TENANCY_OCID" \
    | jq -r '.data[] | select(.status == "READY") | ."region-name"'); then
    fail "OCI: cannot list the tenancy's subscribed regions"
    return
  fi
  if [ -z "$regions" ]; then
    fail "OCI: the tenancy lists no subscribed regions"
    return
  fi
  for region in $regions; do
    prune_oci_region "$region"
  done
}

# --- Alicloud --------------------------------------------------------------

# Retried: mainland endpoints reset connections from CI runners now and then.
ali_cli() {
  local attempt
  for attempt in 1 2 3; do
    if aliyun "$@" --mode AK --access-key-id "$ALICLOUD_ACCESS_KEY" --access-key-secret "$ALICLOUD_SECRET_KEY"; then
      return 0
    fi
    [ "$attempt" -lt 3 ] && sleep "$ALICLOUD_RETRY_SECS"
  done
  return 1
}

# Lists this account's available PREFIX images in one region as one JSON array.
ali_images() {
  local region=$1 page=1 out all='[]' got
  while :; do
    out=$(ali_cli ecs DescribeImages --RegionId "$region" --ImageOwnerAlias self --Status Available \
      --ImageName "${PREFIX}*" --PageSize 100 --PageNumber "$page" 2>"$errfile") || return 1
    all=$(jq -c --argjson acc "$all" '$acc + (.Images.Image // [])' <<<"$out")
    got=$(jq '.Images.Image // [] | length' <<<"$out")
    [ "$got" -lt 100 ] && break
    page=$((page + 1))
  done
  echo "$all"
}

# Tag key on the snapshots backing an image this script deletes; the value is
# the image ID. DeleteImage leaves those snapshots behind, still billed, and
# once the image is gone nothing else leads back to them.
SNAPSHOT_TAG=lantern-box-prune-image

# Returns 0 when the image exists, 1 when it does not, 2 when the lookup failed.
ali_image_exists() {
  local region=$1 id=$2 out n
  out=$(ali_cli ecs DescribeImages --RegionId "$region" --ImageId "$id" --ImageOwnerAlias self) || return 2
  n=$(jq '.Images.Image | length' <<<"$out") || return 2
  [ "$n" -gt 0 ] && return 0
  return 1
}

ali_delete_image() {
  local region=$1 id=$2 name=$3 snaps=$4 users args i rc
  # DeleteImage would also refuse an image a live instance was created from;
  # checking first keeps that a skip instead of a failure.
  if ! users=$(ali_cli ecs DescribeInstances --RegionId "$region" --ImageId "$id" | jq '.TotalCount'); then
    fail "Alicloud $region: cannot check instances using $name ($id)"
    return
  fi
  if [ "$users" != "0" ]; then
    echo "  skipping $name ($id): $users instance(s) use it"
    return
  fi
  if [ "$DRY_RUN" = "1" ]; then
    echo "  would delete $name ($id) and snapshot(s) [$snaps]"
    return
  fi
  if [ -n "$snaps" ]; then
    args=()
    i=1
    for snap in $snaps; do
      args+=("--ResourceId.$i" "$snap")
      i=$((i + 1))
    done
    if ! ali_cli ecs TagResources --RegionId "$region" --ResourceType snapshot "${args[@]}" \
      --Tag.1.Key "$SNAPSHOT_TAG" --Tag.1.Value "$id" >/dev/null; then
      fail "Alicloud $region: cannot tag the snapshots of $name ($id); not deleting it"
      return
    fi
  fi
  echo "  deleting $name ($id)"
  if ! ali_cli ecs DeleteImage --RegionId "$region" --ImageId "$id" >/dev/null; then
    # A retried call fails when the attempt whose response was lost had deleted it.
    rc=0
    ali_image_exists "$region" "$id" || rc=$?
    if [ "$rc" -ne 1 ]; then
      fail "Alicloud $region: failed to delete $name ($id)"
      return
    fi
  fi
  deleted=$((deleted + 1))
}

# Deletes the snapshots tagged by ali_delete_image whose image is gone,
# including any a failed or interrupted earlier run left behind.
ali_sweep_snapshots() {
  local region=$1 out snap image rc attempt ok
  if ! out=$(ali_cli ecs DescribeSnapshots --RegionId "$region" --Tag.1.Key "$SNAPSHOT_TAG" --PageSize 100); then
    fail "Alicloud $region: cannot list snapshots tagged $SNAPSHOT_TAG"
    return
  fi
  while IFS=$'\t' read -r snap image; do
    [ -n "$snap" ] || continue
    rc=0
    ali_image_exists "$region" "$image" || rc=$?
    if [ "$rc" -eq 0 ]; then
      # Its image survived a failed delete; the snapshot goes once the image does.
      continue
    elif [ "$rc" -eq 2 ]; then
      fail "Alicloud $region: cannot check image $image of snapshot $snap"
      continue
    fi
    if [ "$DRY_RUN" = "1" ]; then
      echo "  would delete snapshot $snap of deleted image $image"
      continue
    fi
    # Alicloud keeps the snapshot locked for a short while after DeleteImage.
    ok=false
    for attempt in 1 2 3 4 5 6; do
      if ali_cli ecs DeleteSnapshot --region "$region" --SnapshotId "$snap" >/dev/null; then
        ok=true
        break
      fi
      [ "$attempt" -lt 6 ] && sleep "$SNAPSHOT_RETRY_SECS"
    done
    if $ok; then
      echo "  deleted snapshot $snap of deleted image $image"
    else
      fail "Alicloud $region: failed to delete snapshot $snap of deleted image $image"
    fi
  done < <(jq -r --arg k "$SNAPSHOT_TAG" '.Snapshots.Snapshot[]?
    | [.SnapshotId, (.Tags.Tag[]? | select(.TagKey == $k) | .TagValue)] | @tsv' <<<"$out")
}

prune_alicloud_region() {
  local region=$1 all images created id name snaps
  if ! all=$(ali_images "$region"); then
    # DescribeRegions lists some regions (cn-nanjing, cn-fuzhou) whose endpoints
    # then reject the account. No image can exist there.
    if grep -q InvalidRegionId "$errfile"; then
      echo "Alicloud $region: region not open to this account, skipping"
    else
      fail "Alicloud $region: cannot list images: $(head -c 300 "$errfile")"
    fi
    return
  fi
  images=$(jq -r '.[] | "\(.CreationTime)\t\(.ImageId)\t\(.ImageName)"' <<<"$all")
  echo "Alicloud $region: $(grep -c . <<<"$images" || true) ${PREFIX}* images (keeping $KEEP)"
  while IFS=$'\t' read -r created id name; do
    [ -n "$id" ] || continue
    snaps=$(jq -r --arg id "$id" '.[] | select(.ImageId == $id)
      | [.DiskDeviceMappings.DiskDeviceMapping[]?.SnapshotId | select(. != "")] | join(" ")' <<<"$all")
    ali_delete_image "$region" "$id" "$name" "$snaps"
  done < <(excess <<<"$images")
  ali_sweep_snapshots "$region"
}

prune_alicloud() {
  local regions region
  : "${ALICLOUD_ACCESS_KEY:?}" "${ALICLOUD_SECRET_KEY:?}"
  if ! regions=$(ali_cli ecs DescribeRegions --RegionId "$ALICLOUD_HOME_REGION" | jq -r '.Regions.Region[].RegionId'); then
    fail "Alicloud: cannot list regions"
    return
  fi
  if [ -z "$regions" ]; then
    fail "Alicloud: DescribeRegions returned no regions"
    return
  fi
  for region in $regions; do
    prune_alicloud_region "$region"
  done
}

case "$provider" in
  oci) prune_oci ;;
  alicloud) prune_alicloud ;;
  *)
    echo "usage: $0 oci|alicloud" >&2
    exit 2
    ;;
esac

echo "deleted $deleted image(s), $failures failure(s)"
[ "$failures" -eq 0 ]
