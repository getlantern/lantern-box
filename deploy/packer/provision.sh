#!/usr/bin/env bash
set -euo pipefail

# VERSION is passed as an environment variable by Packer.
: "${VERSION:?VERSION must be set}"

# Use apt-get's built-in lock timeout (seconds) to wait for
# unattended-upgrades to finish on fresh VPS instances.
APT_LOCK_OPTS='-o DPkg::Lock::Timeout=300 -o APT::Get::Lock::Timeout=300'

export DEBIAN_FRONTEND=noninteractive

echo "==> Installing runtime dependencies"
apt-get $APT_LOCK_OPTS update -q
# Keep this package list in sync with Dockerfile
apt-get $APT_LOCK_OPTS install -y -q \
  ca-certificates \
  tzdata \
  nftables

echo "==> Downloading sing-box-extensions .deb from GitHub release"
arch=$(dpkg --print-architecture)  # amd64 or arm64
deb_name="sing-box-extensions_${VERSION}_linux_${arch}.deb"
deb_url="https://github.com/getlantern/lantern-box/releases/download/v${VERSION}/${deb_name}"
echo "    URL: ${deb_url}"
curl -fsSL -o "/tmp/${deb_name}" "${deb_url}"

echo "==> Installing ${deb_name}"
dpkg -i "/tmp/${deb_name}"
rm -f "/tmp/${deb_name}"

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
if ! command -v lantern-box >/dev/null 2>&1; then
  echo "lantern-box not found on PATH" >&2
  exit 1
fi
echo "    lantern-box installed at $(command -v lantern-box)"
echo "==> Done. Image ready."
