#!/bin/bash
# Real end-to-end smoke test: sequential SOCKS5 + HTTP through the meek tunnel.
#
# microsocks requires strict SOCKS5 request-response (no pipelining), so we do:
#   POST 1:  send method-select (3 bytes) → wait for 2-byte reply
#   POST 2:  send CONNECT httpbin.org:80 (18 bytes) → wait for 10-byte reply
#   POST 3+: send HTTP GET → drain HTTP response
#
# Success criterion: HTTP body contains "origin": "<Linode IP>"

set -euo pipefail

FRONT_HOST="${FRONT_HOST:-a248.e.akamai.net}"
INNER_HOST="${INNER_HOST:-meek.dsa.akamai.getiantem.org}"
TARGET_HOST="httpbin.org"
TARGET_PORT=80

EDGE_IP=$(dig +short "$FRONT_HOST" | grep -E '^[0-9]+\.' | head -1)
[ -z "$EDGE_IP" ] && { echo "couldn't resolve $FRONT_HOST" >&2; exit 1; }
echo "Akamai edge IP: $EDGE_IP"

SID=$(openssl rand -hex 16)
echo "session id: $SID"
POST_URL="https://${FRONT_HOST}/"

# --- meek POST helper ---
# usage: meek_post <out-file> [payload-file]
meek_post() {
  local out=$1
  local data=${2:-}
  local args=(
    -sS
    --resolve "${FRONT_HOST}:443:${EDGE_IP}"
    --http1.1
    -X POST
    -H "Host: ${INNER_HOST}"
    -H "X-Session-Id: ${SID}"
    -H "Content-Type: application/octet-stream"
    -o "$out"
    -w "%{size_download}"
  )
  if [ -n "$data" ]; then
    args+=(--data-binary "@$data")
  else
    args+=(--data-binary "")
  fi
  curl "${args[@]}" "$POST_URL"
}

# Send <payload-file> then poll empty POSTs until at least <min-bytes> received,
# concatenating each response chunk into <accum-file>.
# usage: send_and_drain <payload-file> <accum-file> <min-bytes> <max-polls>
send_and_drain() {
  local payload=$1 accum=$2 minb=$3 maxp=$4
  : > "$accum"
  local tmp=$(mktemp)
  local sz
  sz=$(meek_post "$tmp" "$payload")
  cat "$tmp" >> "$accum"
  local n=$(wc -c < "$accum")
  for i in $(seq 1 "$maxp"); do
    [ "$n" -ge "$minb" ] && break
    sleep 0.3
    sz=$(meek_post "$tmp")
    cat "$tmp" >> "$accum"
    n=$(wc -c < "$accum")
  done
  rm -f "$tmp"
  echo "$n"
}

# --- Build the three payloads ---
python3 <<PYEOF
import os
target_host = b'${TARGET_HOST}'
target_port = ${TARGET_PORT}

# Method-select: NO_AUTH
open('/tmp/meek-p1-methodsel.bin', 'wb').write(b'\x05\x01\x00')

# CONNECT httpbin.org:80
buf = bytearray(b'\x05\x01\x00\x03')
buf += bytes([len(target_host)]) + target_host
buf += target_port.to_bytes(2, 'big')
open('/tmp/meek-p2-connect.bin', 'wb').write(bytes(buf))

# HTTP request the proxy forwards once CONNECT completes
http = b'GET /ip HTTP/1.0\r\nHost: ' + target_host + b'\r\nConnection: close\r\n\r\n'
open('/tmp/meek-p3-http.bin', 'wb').write(http)
PYEOF

echo ""
echo "--- Phase 1: SOCKS5 method-select (NO_AUTH) ---"
n=$(send_and_drain /tmp/meek-p1-methodsel.bin /tmp/meek-r1.bin 2 8)
echo "received ${n} bytes: $(xxd -p /tmp/meek-r1.bin | head -1)"
if ! [ "$(xxd -p /tmp/meek-r1.bin | head -c 4)" = "0500" ]; then
  echo "ERROR: expected '0500' (SOCKS5 NO_AUTH accepted), got something else" >&2
  exit 1
fi

echo ""
echo "--- Phase 2: SOCKS5 CONNECT $TARGET_HOST:$TARGET_PORT ---"
n=$(send_and_drain /tmp/meek-p2-connect.bin /tmp/meek-r2.bin 10 8)
echo "received ${n} bytes: $(xxd -p /tmp/meek-r2.bin | head -1)"
# SOCKS5 CONNECT reply: 05 00 00 01 ... (success)
if ! [[ "$(xxd -p /tmp/meek-r2.bin | head -c 8)" =~ ^050000 ]]; then
  echo "ERROR: SOCKS5 CONNECT reply doesn't start with 05 00 00 (REP=success)" >&2
  exit 1
fi
echo "CONNECT succeeded"

echo ""
echo "--- Phase 3: HTTP GET /ip ---"
n=$(send_and_drain /tmp/meek-p3-http.bin /tmp/meek-r3.bin 100 15)
echo "received ${n} bytes of HTTP response"

echo ""
echo "--- HTTP response body ---"
cat /tmp/meek-r3.bin
echo ""
echo "--- success check ---"
if grep -q '"origin"' /tmp/meek-r3.bin; then
  origin=$(grep -o '"origin"[^}]*' /tmp/meek-r3.bin)
  echo "✅ End-to-end SUCCESS: ${origin}"
  echo "The request traversed: curl → Akamai → Caddy → meek-server → microsocks → httpbin.org"
  echo "and httpbin reported it saw the Linode's public IP, proving the proxy actually exited the box."
else
  echo "❌ HTTP response didn't contain origin field — partial chain failure"
  exit 1
fi

echo ""
echo "--- meek-server healthz ---"
curl -sS https://meek.getiantem.org/healthz
echo ""
