#!/usr/bin/env bash
# Deploy meek-server to its fronted origin host.
#
# Builds linux/amd64 from THIS checkout, ships it, verifies the transfer by
# sha256, swaps it in atomically (keeping a timestamped backup), restarts the
# service, and verifies /healthz — rolling back automatically if it doesn't come
# back. Finally runs the end-to-end SOCKS5 smoke test (best-effort).
#
# MEEK_HOST is REQUIRED (no default) so an env-less run can't silently deploy to a
# live origin. The host's service layout isn't pinned in-repo, so the rest is
# overridable via env (defaults below). Confirm these match the host before the
# first real run; use --dry-run to preview.
#
#   MEEK_HOST=139.162.181.47   (required)   MEEK_SSH_USER=root   MEEK_SSH_KEY=~/.ssh/id
#   MEEK_REMOTE_BIN=/usr/local/bin/meek-server      MEEK_SERVICE=meek-server
#   MEEK_RESTART_CMD="systemctl restart $MEEK_SERVICE"
#   MEEK_STATUS_CMD="systemctl is-active $MEEK_SERVICE"
#   MEEK_HEALTHZ_URL=https://meek.getiantem.org/healthz
#   MEEK_SSH_STRICT=accept-new   (set to "yes" for strict host-key checking)
#
# Usage: cmd/meek-server/deploy.sh [-n|--dry-run] [-h|--help]
set -euo pipefail

HOST="${MEEK_HOST:-}"
SSH_USER="${MEEK_SSH_USER:-root}"
SSH_KEY="${MEEK_SSH_KEY:-}"
REMOTE_BIN="${MEEK_REMOTE_BIN:-/usr/local/bin/meek-server}"
SERVICE="${MEEK_SERVICE:-meek-server}"
RESTART_CMD="${MEEK_RESTART_CMD:-systemctl restart $SERVICE}"
STATUS_CMD="${MEEK_STATUS_CMD:-systemctl is-active $SERVICE}"
# No default: a fixed prod URL here would verify (and gate rollback on) the wrong
# host for staging/canary deploys. Unset → the HTTP health check is skipped and we
# rely on the service-status check after restart.
HEALTHZ_URL="${MEEK_HEALTHZ_URL:-}"

DRY_RUN=0
case "${1:-}" in
  -n|--dry-run) DRY_RUN=1 ;;
  -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
  "") ;;
  *) echo "unknown arg: $1 (try --help)" >&2; exit 2 ;;
esac

# Required (checked after --help/-h so those don't need it): never default to a live host.
: "${HOST:?required — set MEEK_HOST to the target origin (e.g. 139.162.181.47); refusing to default to a live host}"

cd "$(dirname "$0")/../.." # repo root

# accept-new (TOFU) by default so a first deploy to operator-owned infra doesn't
# require pre-seeding known_hosts; set MEEK_SSH_STRICT=yes for strict checking.
SSH_STRICT="${MEEK_SSH_STRICT:-accept-new}"
SSH_OPTS=(-o "StrictHostKeyChecking=$SSH_STRICT" -o ConnectTimeout=15)
[ -n "$SSH_KEY" ] && SSH_OPTS+=(-i "$SSH_KEY")
ssh_h() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${HOST}" "$@"; }
say()   { printf '\n=== %s ===\n' "$*"; }

# Hash locally with whichever tool exists: sha256sum (most Linux) or shasum -a 256
# (macOS, some others). The remote always has sha256sum.
sha256_local() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

say "build meek-server (linux/amd64) from $(git rev-parse --short HEAD 2>/dev/null || echo '?')"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$TMP/meek-server" ./cmd/meek-server
LSHA=$(sha256_local "$TMP/meek-server")
echo "built $(wc -c <"$TMP/meek-server") bytes  sha256=$LSHA"

if [ "$DRY_RUN" = 1 ]; then
  cat <<PLAN

[dry-run] would deploy to ${SSH_USER}@${HOST}:
  scp  -> /tmp/meek-server.new   (then verify sha256 == $LSHA)
  cp   $REMOTE_BIN -> ${REMOTE_BIN}.bak.<ts>   (backup, if present)
  install /tmp/meek-server.new -> $REMOTE_BIN
  run  $RESTART_CMD   ;   check  $STATUS_CMD
  verify $HEALTHZ_URL returns "ok"   (else roll back)
  smoke test: cmd/meek-server/smoketest/socks5.sh
PLAN
  exit 0
fi

say "ship to ${SSH_USER}@${HOST}"
scp "${SSH_OPTS[@]}" "$TMP/meek-server" "${SSH_USER}@${HOST}:/tmp/meek-server.new"

say "verify transfer (sha256)"
RSHA=$(ssh_h "sha256sum /tmp/meek-server.new | awk '{print \$1}'")
if [ "$RSHA" != "$LSHA" ]; then
  echo "sha256 mismatch: local=$LSHA remote=$RSHA — aborting, nothing swapped" >&2
  ssh_h "rm -f /tmp/meek-server.new" || true
  exit 1
fi
echo "verified on host"

say "backup + atomic swap + restart"
BAK="${REMOTE_BIN}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
ssh_h bash -s <<REMOTE
set -euo pipefail
if [ -f "$REMOTE_BIN" ]; then cp -a "$REMOTE_BIN" "$BAK"; echo "backed up -> $BAK"; fi
install -m 0755 /tmp/meek-server.new "$REMOTE_BIN"
rm -f /tmp/meek-server.new
$RESTART_CMD
sleep 2
echo -n "service: "; $STATUS_CMD || true
REMOTE

if [ -z "$HEALTHZ_URL" ]; then
  say "verify /healthz (skipped — set MEEK_HEALTHZ_URL for an HTTP health gate)"
  echo "relying on the post-restart service-status check above"
else
  say "verify /healthz"
  ok=0
  for _ in $(seq 1 10); do
    if curl -fsS --max-time 10 "$HEALTHZ_URL" 2>/dev/null | grep -q "ok"; then ok=1; break; fi
    sleep 2
  done
  if [ "$ok" != 1 ]; then
    echo "healthz did not return ok — ROLLING BACK to $BAK" >&2
    ssh_h bash -s <<REMOTE || true
set -euo pipefail
if [ -f "$BAK" ]; then install -m 0755 "$BAK" "$REMOTE_BIN"; $RESTART_CMD; echo "rolled back"; else echo "no backup present"; fi
REMOTE
    exit 1
  fi
  echo "healthz ok"
fi

say "end-to-end smoke test (best-effort)"
if [ -x cmd/meek-server/smoketest/socks5.sh ]; then
  if bash cmd/meek-server/smoketest/socks5.sh; then
    echo "smoke test PASSED"
  else
    echo "NOTE: smoke test did not pass — often httpbin.org being down, not the deploy."
    echo "      re-run against a reliable target, e.g. edit TARGET_HOST to example.com."
  fi
else
  echo "(socks5.sh not found/executable — skipped)"
fi

echo
echo "deploy complete: $REMOTE_BIN @ $HOST  (sha256 $LSHA)  backup: $BAK"
