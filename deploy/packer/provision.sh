#!/usr/bin/env bash
set -euo pipefail

# VERSION is passed as an environment variable by Packer. Under Reflog's
# Option B the packer image no longer installs a specific lantern-box
# release — cloud-init installs the target tag on first boot. VERSION is
# still required and used purely as a label for the built image (see
# lantern-box.pkr.hcl's `image_name = "lantern-box-${var.lantern_box_version}-..."`).
# It stays set so tooling that slices by image label (e.g. the per-provider
# latestImage() helpers in lantern-cloud/cmd/api/vps/*.go) keeps working.
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

# Stop unattended-upgrades so it doesn't race with our apt-get calls here;
# it is purged from the image entirely further down.
echo "==> Stopping unattended-upgrades"
systemctl stop unattended-upgrades.service 2>/dev/null || true
systemctl mask unattended-upgrades.service 2>/dev/null || true
systemctl kill --signal=TERM apt-daily.service 2>/dev/null || true
systemctl kill --signal=TERM apt-daily-upgrade.service 2>/dev/null || true
# Mask the timers too, or the services can restart mid-provision and
# re-acquire the dpkg lock; the masks stay in place in the final image.
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
systemctl mask apt-daily.service apt-daily-upgrade.service apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
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
# Acquire timeouts prevent apt from hanging indefinitely when an OCI region's
# local Ubuntu mirror is slow or unreachable (e.g., the ca-toronto-1 mirror
# became unreachable and, without an acquire timeout, apt hung for 38 minutes
# until the Packer job itself timed out).
APT_OPTS=(
  -o DPkg::Lock::Timeout=300
  -o Acquire::http::Timeout=30
  -o Acquire::https::Timeout=30
  -o Acquire::Retries=3
)

echo "==> Installing runtime dependencies"
apt-get "${APT_OPTS[@]}" update -q
# Keep this package list in sync with Dockerfile
apt-get "${APT_OPTS[@]}" install -y -q \
  ca-certificates \
  tzdata \
  nftables \
  wireguard-tools

# Reflog's Option B (Slack thread ts=1776197690.140869 in
# #infrastructure-and-services, 2026-04-16): the packer image no longer
# bakes in a specific lantern-box version. Cloud-init apt-installs the
# release tag the orchestrator picked for this route — see
# `getlantern/lantern-cloud` cmd/api/vps/cloudinit_packer.go. This
# decouples release cadence (frequent) from base-image cadence (rare).
#
# The packer image contributes: runtime deps (installed above), systemd
# drop-ins for OTel env (below), and /etc/lantern-box and
# /var/lib/lantern-box dirs. The lantern-box .deb itself lands via
# cloud-init on first boot.
#
# Operators: BEFORE building + rolling out new images from this change,
# set bandit_vps_default_release_tag in the lantern-cloud settings table
# (or a per-track override in bandit_vps_image_targets). Without either,
# cloud-init will skip the apt-install step and new VMs will boot
# without a lantern-box binary — `systemctl enable --now lantern-box`
# during config push will then fail. Revert is: re-merge the pre-Option-B
# provision.sh.
arch=$(dpkg --print-architecture)  # amd64 or arm64 — still used below

echo "==> Setting up directories"
mkdir -p /etc/lantern-box /var/lib/lantern-box

echo "==> Installing OTel Collector for host metrics"
otelcol_version="0.120.0"
otelcol_deb="otelcol-contrib_${otelcol_version}_linux_${arch}.deb"
otelcol_url="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${otelcol_version}/${otelcol_deb}"
echo "    URL: ${otelcol_url}"
# Retried with a bounded connect timeout: from some provider regions the first
# connection to github.com can hang for minutes and die with curl exit 28,
# which threw away an otherwise-healthy image build (region me-riyadh-1, run
# 32352757546). Same flags as the CA download below.
curl -fsSL --retry 5 --connect-timeout 10 --max-time 300 \
  -o "/tmp/${otelcol_deb}" "${otelcol_url}"
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

# Grant otelcol-contrib read access to journald so the `journald` receiver
# (see deploy/packer/otelcol.yaml) can ship cloud-init's `logger -t cloud-init …`
# step logs + ERR-trap output to SigNoz. Without this the service would log
# "permission denied" trying to open /run/log/journal and silently drop all
# cloud-init diagnostic messages — which is precisely what happened during
# the May 8 dpkg-flag regression that needed an SSH session to diagnose.
cat > /etc/systemd/system/otelcol-contrib.service.d/journald.conf <<'DROPIN'
[Service]
SupplementaryGroups=systemd-journal
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

# Auto-update is now centrally orchestrated from lantern-cloud — see
# docs/design/central-vps-updates.md. BanditVPSHotSwapWorker SSHes in
# and installs the target release tag; if SSH fails repeatedly,
# BanditVPSAutoreplaceWorker drains the route and a fresh VM provisions
# with the right tag via cloud-init. No more per-host cron, no more
# silent "install failed" errors with no way to identify affected
# hosts (we had 266/hour of those at peak).

# Remove unattended-upgrades from the image entirely. These boxes are
# ephemeral and centrally updated (see central-vps-updates.md note above), and
# on first boot unattended-upgrades races cloud-init's lantern-box install and
# the provision worker's SSH-phase apt-get calls (e.g. InstallDatacapRelease)
# for the dpkg frontend lock — holding it past DPkg::Lock::Timeout fails the
# provision outright. The apt-daily units and timers were masked in the
# early "Stopping unattended-upgrades" block and stay masked in the image.
echo "==> Removing unattended-upgrades"
systemctl unmask unattended-upgrades.service 2>/dev/null || true
apt-get "${APT_OPTS[@]}" purge -y -q unattended-upgrades

echo "==> Disabling SSH password authentication"
# Stock cloud images drop /etc/ssh/sshd_config.d/50-cloud-init.conf with
# PasswordAuthentication yes on first boot; first match in sshd_config.d
# wins, so this override must sort earlier than 50-.
mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/01-lantern-harden.conf <<'SSHHARD'
PasswordAuthentication no
KbdInteractiveAuthentication no
SSHHARD
chmod 644 /etc/ssh/sshd_config.d/01-lantern-harden.conf
echo "    validating sshd config"
sshd -t

echo "==> Creating lantern management user (for Tailscale SSH via Headscale ACL)"
# The Headscale ACL grants group:dev SSH access to tag:external nodes as user "lantern".
# Tailscale SSH looks up the user locally, so it must exist in /etc/passwd.
if id -u lantern >/dev/null 2>&1; then
  echo "    lantern user already exists"
else
  useradd --system --create-home --shell /bin/bash --comment "Lantern management" lantern
fi
# Grant passwordless sudo so operators can perform admin tasks after SSH.
echo "lantern ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/lantern
chmod 440 /etc/sudoers.d/lantern
visudo -cf /etc/sudoers.d/lantern
echo "    lantern user present at $(id lantern)"

echo "==> Installing Tailscale client (for Headscale VPN management)"
# Add Tailscale apt repo — works on Ubuntu 24.04 (noble)
curl -fsSL --retry 5 --connect-timeout 10 --max-time 60 \
  https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg | \
  tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
curl -fsSL --retry 5 --connect-timeout 10 --max-time 60 \
  https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list | \
  tee /etc/apt/sources.list.d/tailscale.list
apt-get "${APT_OPTS[@]}" update -q
apt-get "${APT_OPTS[@]}" install -y -q tailscale
systemctl enable tailscaled
# Do NOT run 'tailscale up' here — cloud-init provides the auth key at runtime.
echo "    tailscale installed at $(command -v tailscale)"

echo "==> Installing Lanternet internal CA certificate"
# The internal ops pipeline (ops.lantr.net) uses a cert signed by the Lanternet
# private CA. Install the CA cert so OTel collectors and the datacap sidecar
# can reach internal services via TLS.
curl -fsSL --retry 5 --connect-timeout 10 --max-time 60 \
  -o /etc/ssl/certs/lanternet.crt \
  "https://privateca-content-62724385-0000-2889-acf2-f403043a1bac.storage.googleapis.com/71406e3543f7e5be892e/ca.crt"
# Verify checksum (cert expires 2032; URL/checksum from lantern-cloud ans/tasks/install-lanternet-ca.yaml)
echo "c9d283c11de3b7d38f1eb38fabcfbcff9b77f302d3eaf506ae691bb14cca792d  /etc/ssl/certs/lanternet.crt" \
  | sha256sum -c -
# Also add to the system CA store so Go's crypto/tls (used by otelcol-contrib
# and lantern-box) trusts it for outgoing HTTPS connections.
mkdir -p /usr/local/share/ca-certificates/lantern
ln -sf /etc/ssl/certs/lanternet.crt /usr/local/share/ca-certificates/lantern/lanternet.crt
update-ca-certificates
echo "    lanternet CA installed"

echo "==> Verifying image contents"
# Under Option B the lantern-box binary is NOT expected in the image —
# cloud-init apt-installs it on first boot. Check the things the packer
# image actually contributes instead: the systemd drop-ins, the data
# dirs, and the sidecars that ARE baked in here.
missing=""
for path in \
  /etc/systemd/system/lantern-box.service.d/otel.conf \
  /etc/systemd/system/otelcol-contrib.service.d/env.conf \
  /etc/systemd/system/otelcol-contrib.service.d/journald.conf \
  /etc/lantern-box \
  /var/lib/lantern-box \
  /etc/otelcol-contrib/config.yaml \
  /etc/ssl/certs/lanternet.crt \
  /etc/ssh/sshd_config.d/01-lantern-harden.conf; do
  [ -e "$path" ] || missing="$missing $path"
done
if [ -n "$missing" ]; then
  echo "image verification failed; missing:$missing" >&2
  exit 1
fi
if ! command -v tailscale >/dev/null 2>&1; then
  echo "tailscale not found on PATH" >&2
  exit 1
fi
if ! command -v otelcol-contrib >/dev/null 2>&1; then
  echo "otelcol-contrib not found on PATH" >&2
  exit 1
fi
# Fail closed on the unattended-upgrades removal: the purge and the early
# masking use `|| true` (units differ across provider base images), so verify
# the end state instead — the package must be gone from dpkg and every
# apt-daily unit must be masked (or absent) so nothing can grab the dpkg lock
# on provisioned boxes.
if dpkg-query -W -f '${db:Status-Status}\n' unattended-upgrades 2>/dev/null | grep -qv '^not-installed$'; then
  echo "unattended-upgrades still present in dpkg — purge above failed" >&2
  exit 1
fi
for unit in apt-daily.service apt-daily-upgrade.service apt-daily.timer apt-daily-upgrade.timer; do
  state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  if [ -n "$state" ] && [ "$state" != "masked" ]; then
    echo "$unit is '$state', expected masked" >&2
    exit 1
  fi
done

# Validate otelcol config at image-build time so malformed YAML, unknown
# receivers/exporters, or missing pipeline references fail the packer
# build instead of silently breaking otelcol-contrib's first start on
# every VPS provisioned from this image. Cheap (sub-second), runs once
# per image build, catches the cheapest class of mistake at the cheapest
# point in the lifecycle.
if ! otelcol-contrib validate --config /etc/otelcol-contrib/config.yaml; then
  echo "otelcol-contrib config validation failed" >&2
  exit 1
fi
echo "    image contents verified"
echo "==> Done. Image ready."
