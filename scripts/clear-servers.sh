#!/usr/bin/env bash

# Script to clear all outbounds and locations from servers.json
# Usage: ./clear-servers.sh

set -e

SERVERS_FILE="/users/shared/lantern/servers.json"
CONFIG_FILE="/users/shared/lantern/config.json"

# Check if servers.json exists
if [ ! -f "$SERVERS_FILE" ]; then
    echo "Error: servers.json not found at $SERVERS_FILE"
    exit 1
fi

# Check if config.json exists
if [ ! -f "$CONFIG_FILE" ]; then
    echo "Error: config.json not found at $CONFIG_FILE"
    exit 1
fi

# Clear outbounds and locations in servers.json using jq
jq '.lantern.outbounds = [] | .lantern.locations = {}' "$SERVERS_FILE" >"${SERVERS_FILE}.tmp"
mv "${SERVERS_FILE}.tmp" "$SERVERS_FILE"

# Clear outbounds and locations in config.json using jq
jq '.ConfigResponse.options.outbounds = [] | .ConfigResponse.outbound_locations = {}' "$CONFIG_FILE" >"${CONFIG_FILE}.tmp"
mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"

echo "✓ Cleared all outbounds and locations from servers.json"
echo "✓ Cleared all outbounds and locations from config.json"
