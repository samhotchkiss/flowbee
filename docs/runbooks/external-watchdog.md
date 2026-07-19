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
`resolved` transitions are sent to the configured webhook.

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
Flowbee persists a transition before calling the webhook. Each notification has an
immutable body and `Idempotency-Key`; a crash after the receiver accepts it safely
retries the same key instead of creating another incident. Continued unhealthy polls
do not produce repeated alerts. Recovery produces exactly one `resolved` notification.

The webhook request uses the same `flowbee.control-alert/v1` outer envelope as the
in-process alert drainer, with `kind: external_deadman` and the firing/resolved incident
inside `payload`. It also uses:

- `Content-Type: application/json`
- `Idempotency-Key: <incident-id>:firing|resolved`
- `X-Flowbee-Signature: sha256=<HMAC-SHA256 of the exact body>`

The receiver must retain idempotency keys across retry windows. State and lock files are
owner-only. The HMAC key is read from an owner-only regular file with symlink following
disabled; it is never accepted as a command-line value or printed by the systemd
template.

## Run it

```bash
install -m 0600 /path/to/provisioned-key ~/.config/flowbee/watchdog.secret
export FLOWBEE_EXTERNAL_WATCHDOG_ID=observer-host-a
export FLOWBEE_WATCHDOG_HEALTH_URL=http://100.x.y.z:7001/healthz
export FLOWBEE_ALERT_WEBHOOK_URL=https://alerts.example.test/flowbee
export FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=$HOME/.config/flowbee/watchdog.secret
flowbee watchdog
```

Use `flowbee watchdog --once` for a one-pass cron probe. The durable file lock rejects
overlapping cron and service invocations. The default state path is
`$XDG_STATE_HOME/flowbee/watchdog.json` or
`~/.local/state/flowbee/watchdog.json`; set `FLOWBEE_WATCHDOG_STATE_FILE` explicitly in
production.

Run `flowbee watchdog --systemd` to print the service account, environment file,
hardened unit, and verification steps. The environment file contains only the secret
file path—not the key.

## Acceptance drill

1. Start the watchdog and confirm a healthy pass in its service log.
2. Stop the Flowbee service. Confirm exactly one `firing` alert with reason
   `process_unreachable`.
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

Do not point the watcher at the dashboard or worker API: `/healthz` is the watcher's
stable HTTP observation contract. The watchdog itself never opens Flowbee's database,
and `/healthz` intentionally returns 503 when a reconciler due clock expires. Do not
replace the structured probe with a status-code-only check: that would reintroduce the
masking failure described above.
