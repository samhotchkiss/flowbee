#!/bin/sh
set -eu

ENV_FILE="${FLOWBEE_SERVE_ENV:-$HOME/.flowbee/serve.env}"
if [ -r "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

: "${FLOWBEE_BIN:=flowbee}"
: "${FLOWBEE_LOG_PATH:=/tmp/flowbee-serve.log}"

mkdir -p "$(dirname "$FLOWBEE_LOG_PATH")"
exec "$FLOWBEE_BIN" serve >>"$FLOWBEE_LOG_PATH" 2>&1

