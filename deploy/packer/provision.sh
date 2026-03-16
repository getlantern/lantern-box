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
killall apt apt-get unattended-upgrade 2>/dev/null || true
# Wait for all apt-family processes to actually exit (up to 2 minutes).
# This is more reliable than a fixed sleep and avoids lock races.
echo "==> Waiting for apt/dpkg processes to finish"
deadline=$((SECONDS + 120))
while [ $SECONDS -lt $deadline ]; do
  if ! pgrep -x "apt|apt-get|dpkg|unattended-upgrade" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
if pgrep -x "apt|apt-get|dpkg|unattended-upgrade" >/dev/null 2>&1; then
  echo "    WARNING: apt processes still running after 2 minutes, proceeding anyway" >&2
fi

# DPkg::Lock::Timeout makes apt-get wait for the dpkg lock instead of failing
# immediately. This is a safety net in case a dpkg process is still finishing.
APT_OPTS=(-o DPkg::Lock::Timeout=300)

echo "==> Installing runtime dependencies"
apt-get "${APT_OPTS[@]}" update -q
# Keep this package list in sync with Dockerfile
apt-get "${APT_OPTS[@]}" install -y -q \
  ca-certificates \
  tzdata \
  nftables

echo "==> Adding Gemfury apt repository"
: "${FURY_READ_TOKEN:?FURY_READ_TOKEN must be set}"
echo "deb [trusted=yes] https://${FURY_READ_TOKEN}@apt.fury.io/getlantern/ /" \
  > /etc/apt/sources.list.d/getlantern.list

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

echo "==> Setting up lantern-box auto-update cron"
# Random delay (0-59 min) prevents all instances from updating simultaneously.
# The cron runs every 6 hours with a random offset per machine.
delay=$((RANDOM % 60))
cat > /etc/cron.d/lantern-box-update <<CRON
SHELL=/bin/bash
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
${delay} */6 * * * root apt-get update -qq -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/getlantern.list -o Dir::Etc::sourceparts="-" && apt-get install -y -qq sing-box-extensions && systemctl restart lantern-box
CRON
chmod 644 /etc/cron.d/lantern-box-update

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
