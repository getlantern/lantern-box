#!/usr/bin/env bash
set -euo pipefail

# VERSION is passed as an environment variable by Packer.
: "${VERSION:?VERSION must be set}"

export DEBIAN_FRONTEND=noninteractive

# Kill unattended-upgrades and wait for any running apt/dpkg to finish.
# Fresh VPS instances often have unattended-upgrades holding apt locks.
echo "==> Stopping unattended-upgrades"
systemctl stop unattended-upgrades.service 2>/dev/null || true
systemctl kill --signal=TERM apt-daily.service 2>/dev/null || true
systemctl kill --signal=TERM apt-daily-upgrade.service 2>/dev/null || true

# Wait for any lingering dpkg/apt processes to exit
while fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock /var/cache/apt/archives/lock >/dev/null 2>&1; do
  echo "    Waiting for apt/dpkg locks..."
  sleep 3
done

echo "==> Installing runtime dependencies"
apt-get update -q
# Keep this package list in sync with Dockerfile
apt-get install -y -q \
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

echo "==> Installing OTel Collector for host metrics"
otelcol_version="0.120.0"
otelcol_deb="otelcol-contrib_${otelcol_version}_linux_${arch}.deb"
otelcol_url="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${otelcol_version}/${otelcol_deb}"
echo "    URL: ${otelcol_url}"
curl -fsSL -o "/tmp/${otelcol_deb}" "${otelcol_url}"
dpkg -i "/tmp/${otelcol_deb}"
rm -f "/tmp/${otelcol_deb}"

# Copy our config into the otelcol-contrib config directory.
# The file is uploaded by the packer file provisioner to /tmp/otelcol.yaml.
cp /tmp/otelcol.yaml /etc/otelcol-contrib/config.yaml

# Create empty env file with restrictive permissions — cloud-init populates it
# with SIGNOZ_INGEST_KEY and OTEL_RESOURCE_ATTRIBUTES before starting the service.
install -m 600 -o root -g root /dev/null /etc/otelcol-contrib/otelcol.env

# Systemd drop-in to load the env file
mkdir -p /etc/systemd/system/otelcol-contrib.service.d
cat > /etc/systemd/system/otelcol-contrib.service.d/env.conf <<'DROPIN'
[Service]
EnvironmentFile=/etc/otelcol-contrib/otelcol.env
DROPIN

systemctl daemon-reload
# Do NOT enable — cloud-init writes env vars first, then enables the service.

echo "==> Verifying installation"
if ! command -v lantern-box >/dev/null 2>&1; then
  echo "lantern-box not found on PATH" >&2
  exit 1
fi
echo "    lantern-box installed at $(command -v lantern-box)"
echo "==> Done. Image ready."
