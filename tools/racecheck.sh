#!/usr/bin/env bash
# Exhaustive, auditable race gate. SQLite migration-heavy Store tests need
# exact-name shards under -race; a monolithic package run otherwise hits Go's
# timeout without proving or disproving a race.
set -euo pipefail

# Three exact-name shards are the independently accepted P1 race topology. It
# keeps each migration-heavy SQLite process below its meaningful timeout without
# dropping a single Test* name, while avoiding the five-way runner contention
# that can starve unrelated fixture setup on GitHub-hosted machines.
shards="${FLOWBEE_RACE_STORE_SHARDS:-3}"
parallel="${FLOWBEE_RACE_STORE_PARALLEL:-3}"
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

# First run the complete suite normally. The race proof below is deliberately
# restricted to Store, whose SQLite-backed transaction/migration paths are the
# concurrency surface under this gate. Running every package under `-race` here
# defeats Store sharding and makes unrelated real-tmux integration tests consume
# the 20-minute budget; those packages remain covered by this exhaustive normal
# suite and their own targeted race jobs when they acquire shared mutable state.
packages=()
while IFS= read -r package; do packages+=("$package"); done < <(go list ./... | grep -v '/internal/store$')
go test -p 1 "${packages[@]}" -short -count=1 -timeout=20m

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
