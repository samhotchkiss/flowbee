# Flowbee v2 Phase 1 canary

Use this runbook to activate one project after the fake/contract suites are green.
The canary does not introduce a second transport: Driver v2.5, including the v2.4
control-principal-origin contract, is the sole managed-session observation and
actuation boundary.

## Preconditions

- `go test ./... -count=1 -timeout=300s` and `go test ./test/acceptance -count=1` pass.
- Both configured Driver endpoints pass metadata, capability, and observation
  conformance. The managed-dedicated endpoint additionally passes isolated
  ensure/control-send/stop conformance; the external/default endpoint passes exact
  non-mutating Interactor adoption and restart recovery. Never run a managed `Ensure`
  as conformance against the external/default domain.
- Driver advertises `features.control_principal_origin=true` from `GET /v2/meta`.
  With the configured Flowbee bearer, `GET /v2/control/capabilities` must return
  `format_version=tmux-driver.control-principal-origin-capability/v1`,
  `principal_id=flowbee-control`, `supported=true`, `authorized=true`, and
  `missing_scopes=[]`. Any mismatch keeps messaging in a durable
  `GAP-FD-003` route-unavailable hold.
- The canary project's exact Interactor binding is active on its configured external
  Driver endpoint. Alerts use the same exact-project route; there is no Matrix,
  provider webhook, or global fallback.
- The endpoint inventory contains both isolated authority domains: the adopted
  Interactor resolves only to an `external/default` endpoint, while the
  Flowbee-created Orchestrator and workers resolve only to a
  `managed_dedicated/non-default` endpoint. Resolution uses the exact
  host/store/domain tuple with no single-default or cross-domain fallback.
- The signed control-alert ingress has a durable project-bound Acceptor, and the
  independent `flowbee watchdog` is running with an explicit matching project ID.
  If Flowbee is unavailable, the watchdog's local durable outbox retains dead-man
  incidents. After Flowbee recovers, signed ingress acceptance creates the durable
  alert obligation and Flowbee projects it to that Interactor.
- The watchdog emits a signed project/identity/target heartbeat on startup and every
  pass. Its two-minute Flowbee lease is a dynamic readiness gate, not a human alert.
  First boot may expose the ingress with readiness degraded only until the first
  heartbeat arrives; it must then converge green without restarting Flowbee.
- Readiness is recomputed on every health request. A stale actor incarnation,
  endpoint-capability revocation, stale capacity generation, unroutable exact seat,
  or expired watchdog lease closes readiness again while reconcilers keep healing.
- Human session, dead-man ingress, and Driver keys are regular owner-only files
  (`0600` or stricter).
- One Codex build seat and a distinct-family review seat have fresh, identity-bound
  capacity observations and exact Driver targets.

Bind each build seat with `flowbee seat bind-driver`; this records only stable Driver
inventory/profile/workspace identities. Session, pane, and agent-run UUIDs arrive in
Driver lifecycle receipts and are never copied from tmux names.

If either endpoint's Flowbee credential lacks `messages:send`, update that daemon with
Driver's supported operation. Never edit the credential JSON by hand:

```bash
td --json auth token-update \
  --config /absolute/path/daemon-tokens.json \
  --principal-id flowbee-control \
  --add-scope messages:send \
  --reload-pid <driver-pid>
```

This preserves the bearer hash, atomically replaces and fsyncs the owner-only token
database, and reloads Driver. Re-probe both endpoints after the reload; a successful
command is not capability proof.

## Enable one project

Set these in the managed `flowbee serve` environment:

```text
FLOWBEE_EPIC_REVIEW_HANDOFF_V2=1
FLOWBEE_PHASE1_DASHBOARD=1
FLOWBEE_CAPACITY_ROUTING_V2=1
FLOWBEE_PRIVATE_ADDR=127.0.0.1:7070
FLOWBEE_DRIVER_ENDPOINTS_FILE=<owner-only exact host/store/domain inventory>
FLOWBEE_EXTERNAL_WATCHDOG_ID=<independent watcher identity>
FLOWBEE_WATCHDOG_PROJECT_ID=<exact canary project id>
FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=<owner-only control-alert ingress HMAC key file>
FLOWBEE_HUMAN_SESSION_KEY_FILE=<owner-only 32+ byte key file>
FLOWBEE_HUMAN_GRANTS_FILE=<owner-only identity@project=role file>
FLOWBEE_CAPACITY_LOCAL_HOST_ID=<stable host id>
FLOWBEE_CAPACITY_COLLECTOR_ID=<enrolled collector identity>
```

Configure the independently supervised watchdog separately; these are not
`flowbee serve` settings:

```text
FLOWBEE_EXTERNAL_WATCHDOG_ID=<independent watcher identity>
FLOWBEE_WATCHDOG_PROJECT_ID=<exact canary project id>
FLOWBEE_WATCHDOG_HEALTH_URL=http://<tailnet-flowbee-host>:7001/healthz
FLOWBEE_ALERT_WEBHOOK_URL=https://<tailnet-flowbee-host>:7443/v1/control-alerts/ingress
FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=<owner-only ingress HMAC key file>
```

The watchdog requires the health and ingress URLs to use the same hostname. Their
schemes and ports may differ; the ingress path is fixed and admits no query or
fragment.

`flowbee serve` does not require or emit to an outbound human webhook. Do not copy the
watchdog target URL into the serve environment and do not install a Matrix or provider
sink. `flowbee serve` and the watchdog use the same canonical owner-only ingress HMAC
key file, while only the watchdog needs the ingress URL. Canary evidence must come from
durable exact-Interactor projection and its Driver grant/receipt or visible route hold.

Keep the private listener on loopback and publish it with Tailscale Serve (for
example, `https://host.tailnet.ts.net:8443 → 127.0.0.1:7070`). Do not carry the
legacy `FLOWBEE_INSECURE=1` setting into the v2 service. Remote workers must use
an explicitly authenticated endpoint when they are enrolled; a public or LAN
bind is not a dashboard-auth substitute.

Start through the service manager. Readiness must remain closed until Driver metadata,
observation snapshot, durable reconcilers, capacity collection, and startup recovery
all succeed. A raw tmux goal watcher, legacy epic supervisor, `epic start/abandon`,
master reply/amend pane delivery, and pane-tail capture are automatically fenced while
v2 is active.

Project readiness is a live projection, not a startup certificate. Every `/healthz`
request rechecks the exact project actor incarnations, per-endpoint control capability,
builder/reviewer topology, fresh identity-bound capacity generation, and signed
watchdog lease. A fact that expires or is fenced after startup must make readiness red;
restored exact evidence must recover without a process restart.

The pinned candidate's live-UDS conformance gate must exercise the same bearers and
daemons that the canary will use. Both endpoints prove exact metadata discovery,
authenticated capability, and observation. Only the managed-dedicated endpoint runs
the isolated ensure/control-origin-send/stop drill; the external/default endpoint
proves exact non-mutating adoption of the existing Interactor. Together the gates must
also prove strict control-grant and control-receipt parsing, an idempotent replay
returning the original receipt, changed-body conflict, route denial and stale-recipient
fencing with zero terminal mutation, and a crash-uncertain receipt that is not blindly
resent. Keep the old listener running until these gates pass. Protocol availability in
Driver's own test suite is necessary but is not evidence that the Flowbee adapter,
configured credentials, and live daemons conform.

Do not provision a `flowbee-control` `driver_session_bindings` row. Direct origin is
the authenticated control principal, not a managed agent session. Production startup
rejects that synthetic binding explicitly. A Claude, Codex, or Grok product-agent
session is not a service principal and must not be used as the author of dashboard,
assignment, or decision-response messages.

Create a ten-minute browser sign-in link without exposing the session key:

```bash
FLOWBEE_WORKER_TOKEN='<token for the explicitly granted identity>' \
  flowbee human login-link --url https://<tailnet-flowbee-origin> --project default
```

If this installation is migrating from an insecure listener and has no enrolled
automation bearer yet, perform exactly one offline bootstrap while `flowbee serve`
is stopped:

```bash
FLOWBEE_CONFIG=~/.flowbee/flowbee.yaml \
FLOWBEE_HUMAN_SESSION_KEY_FILE=~/.flowbee/human-session.key \
FLOWBEE_HUMAN_GRANTS_FILE=~/.flowbee/human-grants \
  flowbee human bootstrap-link --identity sam --project default \
    --url https://<tailnet-flowbee-origin>
```

The command must report an active-server/writer-lock error if the old or new
control plane is still running. It validates the owner-only files and explicit
grant before acquiring the database writer lock, applies pending migrations under
that lock, and commits only a hashed ten-minute bearer. Start the secure candidate
after the command exits, consume the fragment once, and return to the normal
authenticated `human login-link` path. Do not use the loopback development bypass
for a Tailnet listener.

## Acceptance drill

1. Start the candidate before a watchdog lease exists. Confirm the private ingress is
   reachable while `/healthz` reports the exact
   `external_watchdog_lease_missing_or_stale` project hold. Start the watchdog; confirm
   its signed heartbeat turns readiness green without restarting Flowbee and creates
   no `control_alert` or Interactor message.
2. From the dashboard, create an ordinary focused project request.
3. Confirm the durable conversation message and work intent appear before any agent
   claims to have accepted it.
4. If a typed plan/design gate is raised, approve the exact displayed version/hash.
5. Confirm automatic promotion: `ready_for_orchestrator → orchestrating → submitting
   → admitted`; do not issue a second “go.”
6. Confirm Driver records one exact directional grant in strict format
   `tmux-driver.control-route-grant/v1`, with
   `sender_principal_id=flowbee-control` and the exact recipient session, recipient
   pane incarnation, epoch, and bounds. Confirm the send omits
   `on_behalf_of_session_id` and returns one
   `tmux-driver.control-delivery-receipt/v1` receipt carrying that same principal.
   A `submitted` receipt alone must not advance the workflow stage.
7. Kill/restart the Orchestrator after receipt but before processing evidence. Confirm
   Flowbee does not resend blindly and advances only after later exact evidence.
8. Kill/restart Flowbee after epic admission and before builder launch acknowledgement.
   Confirm one epic, one physical seat lease, and one current lifecycle action.
9. Let the build reach real CI green. Interrupt between build completion and review
   dispatch. Confirm the reconciler dispatches exactly one review and commits one
   visible exact-project Interactor alert obligation. If the Interactor route is
   unavailable, confirm a durable hold; restore it and verify one Driver-routed
   delivery rather than a provider webhook.
10. Complete review/merge/cleanup and confirm the seat, pane, worktree, branch, and
   attention projections converge or become an explicit durable hold.
11. Stop only the watchdog. After two minutes, confirm readiness closes with the exact
    stale-lease hold. Restart it and confirm its next durable sequence restores
    readiness without generating a human notification.

## Stop conditions and rollback

Stop the canary for any duplicate effect, cross-project route, stale-incarnation send,
green-by-absence, missing Interactor alert obligation/hold, or state with neither next action nor visible
hold. Preserve the database and Driver archive for audit. Disable Phase 1 and v2 flags
only after the current outboxes are acknowledged or explicitly held; never delete
actions or receipts to make the board look clean. Roll back the session-control boundary
with an explicit `FLOWBEE_EPIC_REVIEW_HANDOFF_V2=0 flowbee serve`; that writer-owned
start persists the rollback. Merely omitting the variable—or setting it only on an
offline `flowbee epic` invocation—does not reopen raw tmux after v2 was activated.
Legacy direct pane operations remain inappropriate for v2-owned epics. Rollback also
must not convert a control-origin action into a legacy `on_behalf_of_session_id` send or
assign it to a synthetic sender session: leave it durably held for a compatible pinned
candidate.

Record the exact artifact, environment, and pre-migration snapshot before stopping
the old listener:

```bash
export FLOWBEE_CONFIG=$HOME/.flowbee/flowbee.yaml
FINAL_BINARY=$HOME/.flowbee/bin/flowbee-p1-<sha256-prefix>
V2_ENV=$HOME/.flowbee/serve-v2-canary.env
ACTIVE_ENV=$HOME/.flowbee/serve.env

"$FINAL_BINARY" doctor --offline
"$FINAL_BINARY" backup --keep 10
shasum -a 256 "$FINAL_BINARY"
cp "$ACTIVE_ENV" "$ACTIVE_ENV.pre-p1"
install -m 600 "$V2_ENV" "$ACTIVE_ENV"
chmod 600 "$ACTIVE_ENV.pre-p1"
```

Write the binary hash and newest backup path into the operator log. Stop the exact
old PID, prove port 7070 is absent, run the offline human bootstrap, then start the
pinned binary from the managed service. Verify that the service uses
`FLOWBEE_SERVE_ENV=$HOME/.flowbee/serve.env` (or explicitly set its override to
`$V2_ENV`) and that the active file's `FLOWBEE_BIN` equals `$FINAL_BINARY`. Never
execute `go run` or a working-tree binary for the canary.

Before the two capability probes pass, the observation/lifecycle-only smoke is
deliberately degraded: `/dashboard` and `/workspace?project=default` return 200,
`/healthz` returns 503 with the exact `GAP-FD-003` `route_unavailable` hold, and
Driver records zero route/message mutations. The independent watchdog recognizes
only that exact structured control-route degradation as process-alive; a DB failure,
overdue reconciler, malformed response, different Driver failure, or unreachable
process still opens a new incident. After the probes pass, do not call messaging live
until the fake suite, real-UDS conformance, direct-origin replay/denial/uncertainty
tests, and this acceptance drill pass against the pinned candidate.

For a preferred rollback, stop the exact candidate PID, restore the saved env file,
leave the additive database intact, and restart the prior pinned binary. Verify its
configured DB and listener through `flowbee doctor --offline`, `/configz`, and a
dashboard smoke. For an emergency data rollback, keep every Flowbee writer stopped
and run:

```bash
FLOWBEE_CONFIG=$HOME/.flowbee/flowbee.yaml \
  "$FINAL_BINARY" restore /absolute/path/to/pre-canary.db --force
```

`restore` first makes an additional safety snapshot and handles the SQLite WAL/SHM
files atomically. Do not restore merely to hide a durable hold or pending action.

Known release-certification gaps are tracked in
`docs/design/flowbee-driver-contract-gaps.md`. GAP-FD-001 (remote worktree removal) and
GAP-FD-002 (remote live provider probes) remain fail-closed. Driver v2.4 implements
the GAP-FD-003 contract, but Flowbee keeps it fail-closed until exact capability proof
and the conformance/acceptance evidence above pass. Do not represent any gap as healthy
capacity, successful cleanup, or live routed messaging merely because a protocol is
available.
