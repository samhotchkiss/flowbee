# Independent Flowbee dead-man

`flowbee watchdog` is a standalone observer for the failure mode where the
control-plane process cannot reach readiness, becomes unreachable, or stops advancing
one of its durable reconciler heartbeats. It has no Flowbee database dependency and
does not access tmux. Run it under a service manager on another tailnet host whenever
possible; running it inside `flowbee serve` would reproduce the shared-fate failure it
exists to detect.

The watchdog polls the control plane's unauthenticated private `/healthz` listener. It
opens an incident when the process cannot be reached, the structured response is
malformed, the database is unhealthy, any reconciler is overdue, or the response has
any other unhealthy condition. Recovery closes the incident. Both the `firing` and
`resolved` transitions, plus one non-alert heartbeat per pass, are sent to Flowbee's
signed private control-alert ingress.

There is one narrow exception while Flowbee is fail-closed because the configured
Driver daemon or bearer has not yet passed the `GAP-FD-003` v2.4 capability proof. A
503 is treated as evidence that the process and reconcilers are alive only when its
structured body says all of the following: `status=degraded`, `db=true`,
`reconciler_overdue=0`, no reconciler health-read error, and `driver_control` is exactly
`required=true`, `available=false`, `status=route_unavailable`, `gap=GAP-FD-003`. This
is a known fail-closed product-route hold, not a process-health failure. Any missing or
different field fails closed. This exception prevents the permanent route hold from
occupying the one active dead-man incident and masking a later process or reconciler
wedge. It does not make Driver messaging available. Once metadata and authenticated
capability proof pass, `/healthz` must no longer use this degradation; production
activation still requires Flowbee's live conformance and canary evidence.

## Durable delivery contract

The local JSON state records the active incident sequence and a notification outbox.
The watchdog persists a transition before calling the ingress. Each notification has
an immutable body and `Idempotency-Key`; a crash after the ingress accepts it safely
retries the same key instead of creating another incident. Continued unhealthy polls
do not produce repeated alerts. Recovery produces exactly one `resolved` notification.
The state is also bound to one explicit stable Flowbee project ID. Changing the
configured project fails closed before probing or draining the queue, so an alert
created for one project can never be replayed into another project's Interactor.
There is no `default` project fallback and the project is never inferred from the
health URL, watchdog ID, state path, or ingress URL.

The ingress request uses the `flowbee.control-alert/v1` outer envelope with
`kind: external_deadman` and the firing/resolved incident inside `payload`. Both the
outer envelope and immutable dead-man payload carry the explicit project ID, and the
publisher refuses to send if they disagree. It also uses:

- `Content-Type: application/json`
- `Idempotency-Key: <incident-id>:firing|resolved`
- `X-Flowbee-Signature: sha256=<HMAC-SHA256 of the exact body>`

Flowbee's ingress must retain idempotency keys across retry windows. Watchdog state and
lock files are owner-only. The HMAC key is read from an owner-only regular file with
symlink following disabled; it is never accepted as a command-line value or printed by
the systemd template.

The production endpoint for this contract is Flowbee's provider-neutral signed
control-alert ingress. Its durable acceptor must commit the exact-project Interactor
notification obligation before acknowledging, and Flowbee delivers that obligation
through the Driver grant/receipt path. There is no Matrix or provider-specific sink.

Each watchdog pass first submits a signed `external_watchdog_heartbeat` through the
same ingress. It carries the exact project, watchdog identity, health target, durable
sequence, and observation time. Flowbee retains the exact signed body and advances a
project-bound lease transactionally, but does **not** create a human alert for a
heartbeat. Phase 1 readiness requires receipt within the last two minutes, so the
watchdog interval is capped at one minute.

On first boot, Flowbee exposes the ingress with readiness degraded while the lease is
absent; it does not deadlock waiting for a request that cannot arrive. The watchdog
retries its durable heartbeat and readiness turns green without a Flowbee restart.
Actor, endpoint-topology, and capacity failures remain hard startup failures.

## Run it

```bash
install -m 0600 /path/to/provisioned-key ~/.config/flowbee/watchdog.secret
export FLOWBEE_EXTERNAL_WATCHDOG_ID=observer-host-a
export FLOWBEE_WATCHDOG_PROJECT_ID=russ
export FLOWBEE_WATCHDOG_HEALTH_URL=http://<tailnet-flowbee-host>:7001/healthz
export FLOWBEE_ALERT_WEBHOOK_URL=https://<tailnet-flowbee-host>:7443/v1/control-alerts/ingress
export FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=$HOME/.config/flowbee/watchdog.secret
flowbee watchdog
```

The health and ingress schemes and ports may differ, but their hostnames must match.
This prevents a misconfigured watchdog from disclosing a signed incident body to an
external receiver before acknowledgement validation can reject it.

Use `flowbee watchdog --once` for a one-pass cron probe. The durable file lock rejects
overlapping cron and service invocations. The default state path is
`$XDG_STATE_HOME/flowbee/watchdog.json` or
`~/.local/state/flowbee/watchdog.json`; set `FLOWBEE_WATCHDOG_STATE_FILE` explicitly in
production.

Watchdog state version 2 introduced the durable project binding. Version-1 state
files fail closed because they cannot prove which project owns a queued alert. Do
not edit them in place or guess a project. Inspect and archive the old state file,
resolve any retained notification deliberately, then start v2 with a new state file
and explicit `FLOWBEE_WATCHDOG_PROJECT_ID`.

Run `flowbee watchdog --systemd` to print the service account, environment file,
hardened unit, and verification steps. The environment file contains only the secret
file path—not the key.

## Acceptance drill

1. Start the watchdog and confirm a healthy pass in its service log. Confirm the
   signed heartbeat advances the exact-project lease without creating a
   `control_alert` or Interactor notification.
2. Stop the Flowbee service. Confirm exactly one `firing` alert with reason
   `process_unreachable` and the exact configured project ID in both envelope and
   payload.
3. Restart the watchdog while Flowbee is still down. Confirm no new incident or alert.
4. Restart Flowbee. Confirm exactly one `resolved` alert carrying the original incident
   ID.
5. Make one reconciler heartbeat overdue and confirm the firing alert names it even
   though the process and HTTP listener remain reachable.
6. While `/healthz` reports only the exact `GAP-FD-003` Driver-control degradation,
   confirm the watchdog reports a healthy pass and no incident. Then stop Flowbee (or
   make a reconciler overdue) and confirm a new `firing` alert is emitted. Restore the
   control-only degraded state and confirm exactly one `resolved` alert; another poll
   must emit nothing.
7. Stop only the watchdog. Within two minutes `/healthz` must degrade with
   `external_watchdog_lease_missing_or_stale`. Restart it and confirm the retained
   sequence advances and readiness recovers without a human alert.

Do not point the watcher at the dashboard or worker API: `/healthz` is the watcher's
stable HTTP observation contract. The watchdog itself never opens Flowbee's database,
and `/healthz` intentionally returns 503 when a reconciler due clock expires. Do not
replace the structured probe with a status-code-only check: that would reintroduce the
masking failure described above.
