#!/bin/sh
set -eu

ENV_FILE="${FLOWBEE_FLEET_ENV:-$HOME/.flowbee/fleet.env}"
if [ -r "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

: "${FLOWBEE_BIN:=flowbee}"
: "${FLOWBEE_LOG_PATH:=/tmp/flowbee-fleet.log}"
: "${FLOWBEE_FLEET_BUILDERS:=4}"
: "${FLOWBEE_FLEET_AGENT:=claude}"

mkdir -p "$(dirname "$FLOWBEE_LOG_PATH")"
exec "$FLOWBEE_BIN" fleet --builders "$FLOWBEE_FLEET_BUILDERS" --agent "$FLOWBEE_FLEET_AGENT" >>"$FLOWBEE_LOG_PATH" 2>&1

