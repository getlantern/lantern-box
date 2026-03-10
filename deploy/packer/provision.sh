#!/usr/bin/env bash
set -euo pipefail

echo "==> Installing runtime dependencies"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q \
  ca-certificates \
  tzdata \
  nftables \
  wireguard-tools \
  curl \
  jq

echo "==> Adding Gemfury apt repo"
# Gemfury hosts the .deb packages produced by GoReleaser
echo "deb [trusted=yes] https://${FURY_TOKEN}@apt.fury.io/getlantern/ /" \
  > /etc/apt/sources.list.d/getlantern.list
apt-get update -q

echo "==> Installing sing-box-extensions (lantern-box) ${LANTERN_BOX_VERSION}"
# GoReleaser publishes the package as "sing-box-extensions"
apt-get install -y -q "sing-box-extensions=${LANTERN_BOX_VERSION}"

# The .deb installs the binary as /usr/bin/sing-box-extensions.
# The systemd service files reference /usr/bin/lantern-box, so create a symlink.
if [ -f /usr/bin/sing-box-extensions ] && [ ! -f /usr/bin/lantern-box ]; then
  ln -s /usr/bin/sing-box-extensions /usr/bin/lantern-box
fi

echo "==> Setting up directories"
mkdir -p /etc/lantern-box /var/lib/lantern-box

# Rename the installed service files to lantern-box for clarity.
# The .deb installs them as sing-box-extensions.service.
for svc in sing-box-extensions.service sing-box-extensions@.service; do
  installed="/usr/lib/systemd/system/${svc}"
  target="/usr/lib/systemd/system/$(echo "$svc" | sed 's/sing-box-extensions/lantern-box/')"
  if [ -f "$installed" ] && [ ! -f "$target" ]; then
    cp "$installed" "$target"
  fi
done

echo "==> Enabling lantern-box service (will start on first boot with config)"
systemctl daemon-reload
systemctl enable lantern-box.service

# Remove the Fury token from the image (security)
rm -f /etc/apt/sources.list.d/getlantern.list

echo "==> Verifying installation"
lantern-box version || sing-box-extensions version || echo "version check skipped"

echo "==> Done. Image ready."
