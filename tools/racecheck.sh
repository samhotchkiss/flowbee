#!/usr/bin/env bash
# Exhaustive, auditable race gate. SQLite migration-heavy Store tests need
# exact-name shards under -race; a monolithic package run otherwise hits Go's
# timeout without proving or disproving a race.
set -euo pipefail

shards="${FLOWBEE_RACE_STORE_SHARDS:-9}"
parallel="${FLOWBEE_RACE_STORE_PARALLEL:-5}"
if ! [[ "$shards" =~ ^[1-9][0-9]*$ ]] || ! [[ "$parallel" =~ ^[1-9][0-9]*$ ]]; then
  echo "racecheck: shard counts must be positive integers" >&2
  exit 2
fi

scratch="$(mktemp -d "${TMPDIR:-/tmp}/flowbee-race.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

go test ./internal/store -list '^Test' | awk '/^Test/ {
  if ($0 !~ /^Test[[:alnum:]_]*$/) {
    print "racecheck: unsupported Store test name " $0 > "/dev/stderr"; exit 2
  }
  print
}' >"$scratch/all.txt"

if [[ ! -s "$scratch/all.txt" ]]; then
  echo "racecheck: Store test discovery returned no Test* names" >&2
  exit 1
fi

for ((i = 0; i < shards; i++)); do
  awk -v i="$i" -v n="$shards" '((NR - 1) % n) == i' "$scratch/all.txt" >"$scratch/$i.txt"
done

# The rest of the repository can use Go's ordinary package-parallel scheduler.
packages=()
while IFS= read -r package; do packages+=("$package"); done < <(go list ./... | grep -v '/internal/store$')
go test "${packages[@]}" -short -race -count=1 -timeout=20m

run_shard() {
  local i="$1" pattern
  pattern="$(paste -sd '|' "$scratch/$i.txt")"
  [[ -n "$pattern" ]] || return 0
  # Store is deliberately not short: its SQLite-backed integration tests are
  # the concurrency proof this gate exists to preserve.
  go test ./internal/store -race -count=1 -timeout=20m -run "^(${pattern})$"
}

for ((start = 0; start < shards; start += parallel)); do
  pids=()
  for ((i = start; i < start + parallel && i < shards; i++)); do
    run_shard "$i" & pids+=("$!")
  done
  for pid in "${pids[@]}"; do wait "$pid"; done
done

found="$(wc -l <"$scratch/all.txt" | tr -d ' ')"
echo "racecheck: green (${found}/${found} Store Test* names across ${shards} exact shards)"
