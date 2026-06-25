#!/usr/bin/env bash
# Deploy meek-server to its fronted origin host.
#
# Builds linux/amd64 from THIS checkout, ships it, verifies the transfer by
# sha256, swaps it in atomically (keeping a timestamped backup), restarts the
# service, and verifies /healthz — rolling back automatically if it doesn't come
# back. Finally runs the end-to-end SOCKS5 smoke test (best-effort).
#
# The repo doesn't pin the host's service layout, so everything is overridable
# via env vars (shown with their defaults below). Confirm these match the host
# before the first real run; use --dry-run to preview.
#
#   MEEK_HOST=139.162.181.47   MEEK_SSH_USER=root   MEEK_SSH_KEY=~/.ssh/id
#   MEEK_REMOTE_BIN=/usr/local/bin/meek-server      MEEK_SERVICE=meek-server
#   MEEK_RESTART_CMD="systemctl restart $MEEK_SERVICE"
#   MEEK_STATUS_CMD="systemctl is-active $MEEK_SERVICE"
#   MEEK_HEALTHZ_URL=https://meek.getiantem.org/healthz
#
# Usage: cmd/meek-server/deploy.sh [-n|--dry-run] [-h|--help]
set -euo pipefail

HOST="${MEEK_HOST:-139.162.181.47}"
SSH_USER="${MEEK_SSH_USER:-root}"
SSH_KEY="${MEEK_SSH_KEY:-}"
REMOTE_BIN="${MEEK_REMOTE_BIN:-/usr/local/bin/meek-server}"
SERVICE="${MEEK_SERVICE:-meek-server}"
RESTART_CMD="${MEEK_RESTART_CMD:-systemctl restart $SERVICE}"
STATUS_CMD="${MEEK_STATUS_CMD:-systemctl is-active $SERVICE}"
HEALTHZ_URL="${MEEK_HEALTHZ_URL:-https://meek.getiantem.org/healthz}"

DRY_RUN=0
case "${1:-}" in
  -n|--dry-run) DRY_RUN=1 ;;
  -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
  "") ;;
  *) echo "unknown arg: $1 (try --help)" >&2; exit 2 ;;
esac

cd "$(dirname "$0")/../.." # repo root

SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)
[ -n "$SSH_KEY" ] && SSH_OPTS+=(-i "$SSH_KEY")
ssh_h() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${HOST}" "$@"; }
say()   { printf '\n=== %s ===\n' "$*"; }

say "build meek-server (linux/amd64) from $(git rev-parse --short HEAD 2>/dev/null || echo '?')"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$TMP/meek-server" ./cmd/meek-server
LSHA=$(shasum -a 256 "$TMP/meek-server" | awk '{print $1}')
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
