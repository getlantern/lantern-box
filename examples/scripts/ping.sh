#!/bin/bash
# ping.sh - Test the CLIENTINFO protocol by sending a mock packet to the proxy
#
# Usage: ./ping.sh [host] [port]
#   host: Proxy host (default: 127.0.0.1)
#   port: Proxy port (default: 8888)
#
# This script sends an HTTP CONNECT request followed by a CLIENTINFO packet,
# which triggers the device_id.connected span in telemetry.

set -e

HOST="${1:-127.0.0.1}"
PORT="${2:-8888}"

echo "Connecting to $HOST:$PORT..."

(
  echo -e "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r"
  sleep 0.5
  echo -n 'CLIENTINFO {"DeviceID":"test-device","Platform":"linux","IsPro":false,"CountryCode":"US","Version":"1.0.0"}'
  sleep 0.5
) | nc -w 2 "$HOST" "$PORT" | head -1

echo "Done. Check collector output for device_id.connected span."
