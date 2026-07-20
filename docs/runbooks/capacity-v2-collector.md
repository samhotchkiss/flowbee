# Capacity v2 live collector activation

The v2 scheduler routes only from one complete active capacity generation. It never
falls back to `worker_accounts.usage_pct` or the legacy `account_windows` fold.

## Host contract

Each intended host must run an authenticated collector adapter implementing
`capacitycollector.HostClient`. Authentication binds the transport peer to the
registered `(collector_id, host_id)`; those values are not accepted from an
untrusted report body. The host receives only its operator-bound seats, calls the
provider's live adapter locally, and returns sanitized observations. It never chooses
a route.

Before activation, every seat must have an operator-approved expected host, provider
account key, and credential-lineage digest. Bind it explicitly with:

```bash
flowbee seat bind-capacity \
  --family codex --codex-home /absolute/path/to/codex-home \
  --host-id control-host-1 --account-key account-uuid \
  --credential-lineage sha256:verified-lineage \
  --reserve-pct 10 --account-max-concurrent 2
```

Use `--config-dir` instead of `--codex-home` for Claude/Grok. A changed login is a
hold requiring explicit rebinding, not an automatic update. The collector never
self-enrolls an observed identity.

Codex collection uses the app-server and waits for the `initialize` response before
any account reads. Grok collection uses the live billing period and preserves weekly
versus monthly semantics. Cache/display-only results remain visible but non-routable.

## Control-plane fold

The control plane groups all enabled seats by host and invokes
`capacitycollector.FleetService.CollectAndCommit`. The fleet service:

1. bounds concurrent hosts and each host bounds provider calls;
2. serializes live probes for the same provider account;
3. honors durable provider/account backoff from `capacity_probe_backoff`;
4. rejects partial, duplicate, wrong-host, or mismatched-generation reports; and
5. calls `CommitCapacityGeneration` exactly once after every expected host returned.

Never commit a per-host generation directly. That would temporarily erase the other
hosts from the active generation.

When `FLOWBEE_CAPACITY_ROUTING_V2=1`, `foldSeatCapacity` is disabled, so
`UpsertAccountLimits` cannot compete with the v2 projection writer.

`flowbee serve` currently wires the production local-host adapter. It collects once
before readiness, then on the configured cadence, and records its supervised-loop
heartbeat. Each complete commit updates the append-only observations, seat/account
projections, per-seat health, and active-generation pointer in one transaction.

Configure the local adapter with:

```bash
export FLOWBEE_EPIC_REVIEW_HANDOFF_V2=1
export FLOWBEE_CAPACITY_ROUTING_V2=1
export FLOWBEE_CAPACITY_LOCAL_HOST_ID=control-host-1
export FLOWBEE_CAPACITY_COLLECTOR_ID=capacity-local-1
export FLOWBEE_ENROLLED_IDENTITIES=capacity-local-1
export FLOWBEE_CAPACITY_COLLECT_INTERVAL=2m
```

The collector identity must be in the existing enrolled-identity allowlist. The
cadence must be greater than zero and at most four minutes, leaving the reconciler
watchdog time to surface a missed collection before the five-minute route-freshness
window expires.

There is currently no authenticated control-plane-to-remote-host live provider probe
in the fleet API or current Driver v2 SDK. Therefore every enabled seat must be physically
local (`seats.box=''`) for this activation. A remote seat fails serve readiness with
`GAP-FD-002`. See [Flowbee ↔ Driver contract gaps](../design/flowbee-driver-contract-gaps.md).
Do not replace this with the legacy SSH cache probe or a routed terminal message.

## Zero-pool recovery

The scheduling/reconciliation pass supplies project-scoped required build, review,
and operations pools to `Store.ReconcileCapacityPools`. Queued work with zero
routable seats immediately opens durable `capacity_pool_exhausted` attention. If the
condition survives the configured threshold, one episode-scoped alert obligation is
queued for the exact project Interactor. If that Driver route is unavailable, the
obligation remains in a visible durable hold. A fresh routable generation resolves
the attention and advances the control-event digest.

## Activation gates

- every intended host has an authenticated collector client;
- every enabled Codex/Grok seat has exact expected identity and lineage bindings;
- one complete shadow generation contains one observation per enabled seat;
- missing/old/live-unavailable observations are visibly held, never routed;
- a forced provider outage survives process restart without a probe storm;
- a queued zero-capacity pool produces attention and exactly one project-Interactor
  alert obligation (or a visible route hold);
- the legacy fold regression test stays green with the v2 flag enabled.

The current local-only release gate additionally requires that every enabled seat has
`box=''` and `expected_host_id` equal to `FLOWBEE_CAPACITY_LOCAL_HOST_ID`. Re-run the
binding command after an intentional login/credential-lineage change, then restart or
wait for the next complete generation.
