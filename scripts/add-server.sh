#!/usr/bin/env bash

# Script to add a singbox outbound and its location to servers.json
# Usage: ./add-server.sh <outbound.json> <location.json>
# Or: ./add-server.sh <outbound.json> <country> <city> <latitude> <longitude> <country_code>

set -e

SERVERS_FILE="/users/shared/lantern/servers.json"
CONFIG_FILE="/users/shared/lantern/config.json"

if [ $# -lt 2 ]; then
    echo "Usage: $0 <outbound.json> <location.json>"
    echo "   Or: $0 <outbound.json> <country> <city> <latitude> <longitude> <country_code>"
    echo ""
    echo "Example outbound.json:"
    echo '{"type":"shadowsocks","tag":"shadowsocks-out-server1","server":"1.2.3.4","server_port":49749,"method":"chacha20-ietf-poly1305","password":"password123"}'
    echo ""
    echo "Example location.json:"
    echo '{"country":"U.S.A.","city":"New York","latitude":40.7128,"longitude":-74.0060,"country_code":"US"}'
    exit 1
fi

OUTBOUND_FILE=$1

# Check if outbound file exists
if [ ! -f "$OUTBOUND_FILE" ]; then
    echo "Error: Outbound file '$OUTBOUND_FILE' not found"
    exit 1
fi

# Read the outbound JSON
OUTBOUND=$(cat "$OUTBOUND_FILE")

# Extract the tag from the outbound
TAG=$(echo "$OUTBOUND" | jq -r '.tag')

if [ -z "$TAG" ] || [ "$TAG" = "null" ]; then
    echo "Error: Outbound must have a 'tag' field"
    exit 1
fi

# Parse location data
if [ $# -eq 2 ]; then
    # Location provided as JSON file
    LOCATION_FILE=$2
    if [ ! -f "$LOCATION_FILE" ]; then
        echo "Error: Location file '$LOCATION_FILE' not found"
        exit 1
    fi
    LOCATION=$(cat "$LOCATION_FILE")
elif [ $# -eq 6 ]; then
    # Location provided as individual arguments
    COUNTRY=$2
    CITY=$3
    LATITUDE=$4
    LONGITUDE=$5
    COUNTRY_CODE=$6

    LOCATION=$(jq -n \
        --arg country "$COUNTRY" \
        --arg city "$CITY" \
        --argjson lat "$LATITUDE" \
        --argjson lon "$LONGITUDE" \
        --arg cc "$COUNTRY_CODE" \
        '{country: $country, city: $city, latitude: $lat, longitude: $lon, country_code: $cc}')
else
    echo "Error: Invalid number of arguments"
    exit 1
fi

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

# Backup the original files
cp "$SERVERS_FILE" "${SERVERS_FILE}.backup"
cp "$CONFIG_FILE" "${CONFIG_FILE}.backup"

# Update the servers.json file using jq
jq --argjson outbound "$OUTBOUND" \
    --arg tag "$TAG" \
    --argjson location "$LOCATION" \
    '.lantern.outbounds += [$outbound] |
    .lantern.locations[$tag] = $location' \
    "$SERVERS_FILE" >"${SERVERS_FILE}.tmp"

# Move the temporary file to replace the original
mv "${SERVERS_FILE}.tmp" "$SERVERS_FILE"

# Update the config.json file using jq
jq --argjson outbound "$OUTBOUND" \
    --arg tag "$TAG" \
    --argjson location "$LOCATION" \
    '.ConfigResponse.options.outbounds += [$outbound] |
    .ConfigResponse.outbound_locations[$tag] = $location' \
    "$CONFIG_FILE" >"${CONFIG_FILE}.tmp"

# Move the temporary file to replace the original
mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"

echo "✓ Successfully added outbound with tag '$TAG' to both files"
echo "✓ Added location entry for '$TAG' to both files"
echo "Backups saved to ${SERVERS_FILE}.backup and ${CONFIG_FILE}.backup"
