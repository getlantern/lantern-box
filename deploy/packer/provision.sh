#!/usr/bin/env bash
set -euo pipefail

echo "==> Installing runtime dependencies"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
# Keep this package list in sync with Dockerfile
apt-get install -y -q \
  ca-certificates \
  tzdata \
  nftables \
  wireguard-tools

echo "==> Adding Gemfury apt repo"
# Gemfury hosts the .deb packages produced by GoReleaser.
# trusted=yes is required because Gemfury does not provide GPG-signed
# repos. The token is removed from the image after install (see below).
echo "deb [trusted=yes] https://${FURY_TOKEN}@apt.fury.io/getlantern/ /" \
  > /etc/apt/sources.list.d/getlantern.list
apt-get update -q

echo "==> Installing sing-box-extensions (lantern-box)"
# GoReleaser publishes the package as "sing-box-extensions".
# Don't pin an exact version — nfpm appends a release suffix (e.g. -1)
# that makes exact matching fragile. The Gemfury repo only has one version.
apt-get install -y -q sing-box-extensions

# Remove the Fury repo and credentials from the image immediately.
rm -f /etc/apt/sources.list.d/getlantern.list

# The .deb installs the binary as /usr/bin/sing-box-extensions.
# The systemd service files reference /usr/bin/lantern-box, so create a symlink.
ln -sf /usr/bin/sing-box-extensions /usr/bin/lantern-box

echo "==> Setting up directories"
mkdir -p /etc/lantern-box /var/lib/lantern-box

# Symlink installed service files to lantern-box names.
# Using symlinks (not copies) so updates to the .deb package are reflected.
for svc in sing-box-extensions.service sing-box-extensions@.service; do
  installed="/usr/lib/systemd/system/${svc}"
  target="/usr/lib/systemd/system/$(echo "$svc" | sed 's/sing-box-extensions/lantern-box/')"
  if [ -f "$installed" ]; then
    ln -sf "$installed" "$target"
  fi
done

systemctl daemon-reload

# Do NOT enable the service here — it would start on boot before cloud-init
# writes the config, causing a startup failure loop. Cloud-init should run:
#   systemctl enable --now lantern-box

echo "==> Verifying installation"
lantern-box version || sing-box-extensions version

echo "==> Done. Image ready."
