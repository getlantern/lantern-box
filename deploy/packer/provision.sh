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

echo "==> Downloading lantern-box .deb from GitHub release"
arch=$(dpkg --print-architecture)  # amd64 or arm64
deb_name="lantern-box_${VERSION}_linux_${arch}.deb"
deb_url="https://github.com/getlantern/lantern-box/releases/download/v${VERSION}/${deb_name}"
echo "    URL: ${deb_url}"
curl -fsSL -o "/tmp/${deb_name}" "${deb_url}"

echo "==> Installing ${deb_name}"
apt-get "${APT_OPTS[@]}" install -y -q "/tmp/${deb_name}"
rm -f "/tmp/${deb_name}"

echo "==> Setting up directories"
mkdir -p /etc/lantern-box /var/lib/lantern-box

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
# with OTEL_RESOURCE_ATTRIBUTES before starting the service.
install -m 600 -o root -g root /dev/null /etc/otelcol-contrib/otelcol.env

# Systemd drop-in to load the env file
mkdir -p /etc/systemd/system/otelcol-contrib.service.d
cat > /etc/systemd/system/otelcol-contrib.service.d/env.conf <<'DROPIN'
[Service]
EnvironmentFile=/etc/otelcol-contrib/otelcol.env
DROPIN

# Create empty env file for lantern-box OTel — cloud-init populates it
# with OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS, and
# OTEL_RESOURCE_ATTRIBUTES before starting the service.
install -m 600 -o root -g root /dev/null /etc/lantern-box/otel.env

# Systemd drop-in to load OTel env vars into lantern-box
mkdir -p /etc/systemd/system/lantern-box.service.d
cat > /etc/systemd/system/lantern-box.service.d/otel.conf <<'DROPIN'
[Service]
EnvironmentFile=/etc/lantern-box/otel.env
DROPIN

systemctl daemon-reload
# Do NOT enable — cloud-init writes env vars first, then enables the service.

echo "==> Setting up lantern-box auto-update (via GitHub Releases)"
mkdir -p /var/log/lantern-box
cat > /usr/local/bin/lantern-box-update <<'SCRIPT'
#!/bin/bash
set -uo pipefail
# NOTE: no -e — we handle errors explicitly so we can log them to SigNoz.

LOG_FILE="/var/log/lantern-box/update.log"

# Structured JSON log line for the OTel filelog receiver → SigNoz pipeline.
# Uses python3 json.dumps for safe escaping of all dynamic fields.
log_json() {
  local level="$1" msg="$2"
  local ts json_line
  ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  json_line=$(
    TS="$ts" \
    LEVEL="$level" \
    MSG="$msg" \
    CURRENT_VER="${current_ver:-unknown}" \
    LATEST_VER="${latest_ver:-unknown}" \
    python3 -c '
import json, os
print(json.dumps({
    "timestamp": os.environ["TS"],
    "severity": os.environ["LEVEL"],
    "body": os.environ["MSG"],
    "source": "lantern-box-update",
    "current_ver": os.environ.get("CURRENT_VER", "unknown"),
    "latest_ver": os.environ.get("LATEST_VER", "unknown"),
}, ensure_ascii=False))
'
  )
  printf '%s\n' "$json_line" >> "$LOG_FILE"
}

# Prevent overlapping runs — cron may start a new instance while the
# previous one is still sleeping or installing.
exec 9>/var/lock/lantern-box-update.lock
if ! flock -n 9; then
  exit 0
fi

# Derive a per-machine sleep (0-599s) from machine-id so instances stagger naturally
# within the 10-minute check interval.
sleep $(( $(cksum /etc/machine-id | cut -d' ' -f1) % 600 ))

arch=$(dpkg --print-architecture)
current_ver=$(dpkg-query -W -f='${Version}' lantern-box 2>/dev/null || echo "none")

# Fetch latest release tag by following the /releases/latest redirect and
# extracting the tag from the final URL. This is more robust than reading
# %{redirect_url} from a HEAD request, which can fail when curl is behind
# a transparent proxy or CDN that follows the 302 before curl sees it.
curl_err=$(mktemp)
final_url=$(curl -fsSL --retry 3 --max-time 30 -o /dev/null -w '%{url_effective}' \
  https://github.com/getlantern/lantern-box/releases/latest 2>"$curl_err") || true
curl_stderr=$(cat "$curl_err" 2>/dev/null)
rm -f "$curl_err"
latest_tag="${final_url##*/}"

if [ -z "$latest_tag" ] || [ "$latest_tag" = "latest" ]; then
  log_json "ERROR" "failed to fetch latest release tag (final_url=${final_url:-empty}, curl_err=${curl_stderr:-none})"
  exit 1
fi

latest_ver="${latest_tag#v}"

if [ "$current_ver" = "$latest_ver" ]; then
  exit 0
fi

log_json "INFO" "update available: ${current_ver} -> ${latest_ver}"

deb_name="lantern-box_${latest_ver}_linux_${arch}.deb"
deb_url="https://github.com/getlantern/lantern-box/releases/download/${latest_tag}/${deb_name}"

tmpfile=$(mktemp /tmp/lantern-box-update-XXXXXX.deb) || tmpfile=""
if [ -z "$tmpfile" ]; then
  log_json "ERROR" "failed to create temporary file for downloading ${deb_url}"
  exit 1
fi
trap 'rm -f "$tmpfile"' EXIT

if ! curl -fsSL --retry 3 --max-time 120 -o "$tmpfile" "${deb_url}"; then
  log_json "ERROR" "failed to download ${deb_url}"
  exit 1
fi

if ! dpkg -o DPkg::Lock::Timeout=120 -i "$tmpfile"; then
  apt-get -o DPkg::Lock::Timeout=120 update -qq && apt-get -o DPkg::Lock::Timeout=120 install -f -y -qq
fi

new_ver=$(dpkg-query -W -f='${Version}' lantern-box 2>/dev/null || echo "none")
if [ "$new_ver" != "$latest_ver" ]; then
  log_json "ERROR" "install failed: expected ${latest_ver} but got ${new_ver}"
  exit 1
fi

log_json "INFO" "upgraded ${current_ver} -> ${new_ver}, restarting service"
if ! systemctl restart lantern-box; then
  log_json "ERROR" "failed to restart lantern-box service after upgrade to ${new_ver}"
  exit 1
fi
SCRIPT
chmod 755 /usr/local/bin/lantern-box-update

# Rotate the update log so it doesn't grow unbounded.
cat > /etc/logrotate.d/lantern-box <<'LOGROTATE'
/var/log/lantern-box/*.log {
  daily
  rotate 7
  compress
  missingok
  notifempty
  copytruncate
}
LOGROTATE

cat > /etc/cron.d/lantern-box-update <<'CRON'
SHELL=/bin/bash
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
MAILTO=""
*/10 * * * * root /usr/local/bin/lantern-box-update 2>&1 | logger -t lantern-box-update
CRON
chmod 644 /etc/cron.d/lantern-box-update

# Re-enable unattended-upgrades so the final image receives security updates.
systemctl unmask unattended-upgrades.service 2>/dev/null || true
systemctl enable unattended-upgrades.service 2>/dev/null || true

echo "==> Installing Tailscale client (for Headscale VPN management)"
# Add Tailscale apt repo — works on Ubuntu 24.04 (noble)
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg | \
  tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list | \
  tee /etc/apt/sources.list.d/tailscale.list
apt-get "${APT_OPTS[@]}" update -q
apt-get "${APT_OPTS[@]}" install -y -q tailscale
systemctl enable tailscaled
# Do NOT run 'tailscale up' here — cloud-init provides the auth key at runtime.
echo "    tailscale installed at $(command -v tailscale)"

echo "==> Verifying installation"
if ! command -v lantern-box >/dev/null 2>&1; then
  echo "lantern-box not found on PATH" >&2
  exit 1
fi
echo "    lantern-box installed at $(command -v lantern-box)"
echo "==> Done. Image ready."
