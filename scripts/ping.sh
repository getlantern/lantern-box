#!/usr/bin/env bash
#
# Mock a lantern-client connection through the HTTP proxy.
#
# Usage: ./ping.sh [host:-127.0.0.1] [port:-8888]
#
# Opens an HTTP CONNECT tunnel, then sends a CLIENTINFO preamble
# through it, matching the injector protocol used by real clients.

set -e

HOST="${1:-127.0.0.1}"
PORT="${2:-8888}"
TARGET="example.com:443"

CLIENTINFO='CLIENTINFO {"DeviceID":"test-device","Platform":"linux","IsPro":false,"CountryCode":"US","Version":"1.0.0"}'

echo "Connecting to $HOST:$PORT..."

exec 3<>/dev/tcp/"$HOST"/"$PORT"

# Send HTTP CONNECT to establish tunnel.
printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' "$TARGET" "$TARGET" >&3
read -r -t 5 http_response <&3

if [[ "$http_response" != *"200"* ]]; then
  echo "CONNECT failed: $http_response" >&2
  exec 3>&-
  exit 1
fi
# Drain remaining headers (blank line terminates).
while read -r -t 2 line <&3; do
  [[ "$line" == $'\r' || -z "$line" ]] && break
done
echo "Tunnel established."

# Send CLIENTINFO through the tunnel and wait for OK.
printf '%s' "$CLIENTINFO" >&3
read -r -n 2 -t 5 response <&3
exec 3>&-

if [ "$response" = "OK" ]; then
  echo "OK — device_id.connected span emitted."
else
  echo "CLIENTINFO failed: ${response:-<empty>}" >&2
  exit 1
fi
