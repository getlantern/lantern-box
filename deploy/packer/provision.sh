#!/usr/bin/env bash
set -euo pipefail

# VERSION is passed as an environment variable by Packer.
: "${VERSION:?VERSION must be set}"

export DEBIAN_FRONTEND=noninteractive

# Wait for cloud-init to finish — it triggers apt operations on fresh VPS instances.
if command -v cloud-init >/dev/null 2>&1; then
  echo "==> Waiting for cloud-init to finish (max 5 minutes)"
  if timeout 300s cloud-init status --wait; then
    echo "    cloud-init finished successfully"
  else
    echo "    WARNING: cloud-init wait exited with code $?" >&2
  fi
fi

# Stop unattended-upgrades so it doesn't race with our apt-get calls.
# We mask it during provisioning but re-enable at the end of the script
# so the final image still receives automatic security updates.
echo "==> Stopping unattended-upgrades"
systemctl stop unattended-upgrades.service 2>/dev/null || true
systemctl mask unattended-upgrades.service 2>/dev/null || true
systemctl kill --signal=TERM apt-daily.service 2>/dev/null || true
systemctl kill --signal=TERM apt-daily-upgrade.service 2>/dev/null || true
# Kill any lingering apt/unattended-upgrade processes (but not dpkg — killing
# dpkg mid-transaction can corrupt the package database).
killall -9 apt-get unattended-upgrade 2>/dev/null || true
sleep 2

# Use apt-get's built-in lock timeout (wait up to 5 minutes for locks to clear)
# instead of a fragile fuser loop that can race between update and install.
APT_OPTS=(-o DPkg::Lock::Timeout=300)

echo "==> Installing runtime dependencies"
apt-get "${APT_OPTS[@]}" update -q
# Keep this package list in sync with Dockerfile
apt-get "${APT_OPTS[@]}" install -y -q \
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
apt-get "${APT_OPTS[@]}" install -y -q "/tmp/${deb_name}"
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
apt-get "${APT_OPTS[@]}" install -y -q "/tmp/${otelcol_deb}"
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

# Re-enable unattended-upgrades so the final image receives security updates.
systemctl unmask unattended-upgrades.service 2>/dev/null || true
systemctl enable unattended-upgrades.service 2>/dev/null || true

echo "==> Verifying installation"
if ! command -v lantern-box >/dev/null 2>&1; then
  echo "lantern-box not found on PATH" >&2
  exit 1
fi
echo "    lantern-box installed at $(command -v lantern-box)"
echo "==> Done. Image ready."
