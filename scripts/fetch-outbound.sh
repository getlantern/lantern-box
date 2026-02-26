#!/usr/bin/env bash

set -ef -o pipefail

# to disable config fetches: 
# launchctl setenv RADIANCE_DISABLE_FETCH_CONFIG true

LANTERN_CLOUD=$([[ ! -z "$LANTERN_CLOUD" ]] && echo "$LANTERN_CLOUD" || echo "../lantern-cloud")
source $LANTERN_CLOUD/prod.env

PROXY="${1:?please specify the proxy IP}"

echo "getting sing-box outbound options for ${PROXY}"
opts=$($LANTERN_CLOUD/bin/lc route dump-config $PROXY | jq '.')

# get the outbound info
sb_opts=$(echo ${opts} | jq '.connectSingbox.options')

sb_opts=${sb_opts//\\/}
sb_opts=${sb_opts#"\""}
sb_opts=${sb_opts%"\""}
outbound=$(echo ${sb_opts} | jq '.outbounds[0]')
tag=$(echo ${outbound} | jq -r '.tag')

servername=$(echo ${opts} | jq -r '.name')
newtag=$tag-$servername
echo "Updating outbound tag to ${newtag}"

outbound=$(echo ${outbound} | jq --arg newtag "$newtag" '.tag = $newtag')

# get the server location info
server=$(echo ${opts} | jq '.location')

CONFIG_DIR=lc-configs
mkdir -p $CONFIG_DIR
echo "${outbound}" > $CONFIG_DIR/out-$PROXY.json
echo "${server}" > $CONFIG_DIR/location-$PROXY.json

echo "${config}"
sudo ./add-server.sh $CONFIG_DIR/out-$PROXY.json $CONFIG_DIR/location-$PROXY.json