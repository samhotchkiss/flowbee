#!/usr/bin/env bash
# Read-only gate for a Flowbee v2 local-only canary.
#
# This script deliberately never opens a Flowbee database, starts a listener,
# writes a Driver grant, or runs a Driver lifecycle operation.  It proves only
# the pinned binary identity and the read/capability/observation portion of the
# Driver live-UDS contract.  The managed lifecycle/control-origin drill remains
# an explicit, separately authorized operator step in the runbook.
set -euo pipefail

fail() {
  printf 'phase1 canary preflight: %s\n' "$*" >&2
  exit 1
}

need() {
  local name=$1
  [[ -n ${!name:-} ]] || fail "${name} is required"
}

owner_only_regular_file() {
  local path=$1 label=$2 mode
  [[ -f "$path" && ! -L "$path" ]] || fail "${label} must be a regular non-symlink file: ${path}"
  if stat -f '%Lp' "$path" >/dev/null 2>&1; then
    mode=$(stat -f '%Lp' "$path") # macOS/BSD
  else
    mode=$(stat -c '%a' "$path")  # GNU
  fi
  (( (8#$mode & 8#077) == 0 )) || fail "${label} is not owner-only (${mode}): ${path}"
}

run_read_only_driver_gate() {
  local label=$1 socket=$2 token=$3
  [[ -S "$socket" ]] || fail "${label} socket is not a Unix socket: ${socket}"
  owner_only_regular_file "$token" "${label} Driver token"
  printf '\n== %s: read/capability/observation conformance ==\n' "$label"
  FLOWBEE_DRIVER_LIVE_TEST=1 \
  FLOWBEE_DRIVER_SOCKET="$socket" \
  FLOWBEE_DRIVER_TOKEN_FILE="$token" \
    go test ./internal/driver -run '^TestLiveDriverV24Conformance$' -count=1 -v
}

need FLOWBEE_CANARY_BINARY
need FLOWBEE_CANARY_BINARY_SHA256
need FLOWBEE_CANARY_SOURCE_COMMIT
need FLOWBEE_DRIVER_EXTERNAL_SOCKET
need FLOWBEE_DRIVER_EXTERNAL_TOKEN_FILE
need FLOWBEE_DRIVER_MANAGED_SOCKET
need FLOWBEE_DRIVER_MANAGED_TOKEN_FILE

[[ -x "$FLOWBEE_CANARY_BINARY" && ! -L "$FLOWBEE_CANARY_BINARY" ]] ||
  fail "FLOWBEE_CANARY_BINARY must be an executable non-symlink pinned artifact"
[[ -f go.mod ]] || fail "run from the exact Flowbee source checkout that contains the conformance test"
source_head=$(git rev-parse HEAD 2>/dev/null) || fail "resolve source checkout commit"
[[ "$source_head" == "$FLOWBEE_CANARY_SOURCE_COMMIT" ]] ||
  fail "source checkout commit does not match FLOWBEE_CANARY_SOURCE_COMMIT: got ${source_head}"
[[ "$FLOWBEE_DRIVER_EXTERNAL_SOCKET" != "$FLOWBEE_DRIVER_MANAGED_SOCKET" ]] ||
  fail "external/default and managed_dedicated sockets must be distinct"

actual_sha=$(shasum -a 256 "$FLOWBEE_CANARY_BINARY" | awk '{print $1}')
[[ "$actual_sha" == "$FLOWBEE_CANARY_BINARY_SHA256" ]] ||
  fail "pinned binary SHA-256 mismatch: got ${actual_sha}"
binary_source_commit=$("$FLOWBEE_CANARY_BINARY" version --json 2>/dev/null |
  sed -n 's/.*"source_commit":"\([0-9a-f][0-9a-f]*\)".*/\1/p')
[[ "$binary_source_commit" == "$FLOWBEE_CANARY_SOURCE_COMMIT" ]] ||
  fail "pinned binary source commit does not match FLOWBEE_CANARY_SOURCE_COMMIT: got ${binary_source_commit:-missing}"

printf '== pinned artifact ==\n%s\nsha256 %s\n' \
  "$("$FLOWBEE_CANARY_BINARY" version)" "$actual_sha"
printf '%s\n' 'No Flowbee database, listener, Driver lifecycle, grant, or message mutation will run.'

run_read_only_driver_gate external/default \
  "$FLOWBEE_DRIVER_EXTERNAL_SOCKET" "$FLOWBEE_DRIVER_EXTERNAL_TOKEN_FILE"
run_read_only_driver_gate managed_dedicated \
  "$FLOWBEE_DRIVER_MANAGED_SOCKET" "$FLOWBEE_DRIVER_MANAGED_TOKEN_FILE"

printf '\nphase1 canary preflight: GREEN (read-only gates only)\n'
