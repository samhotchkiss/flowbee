# Flowbee ↔ Driver contract gaps

This file records integration gaps discovered while implementing the binding
Flowbee v2 control-plane design. It is not a temporary protocol and does not
authorize Flowbee to use raw tmux, `tmux-send`, SSH shell mutation, inferred
paths, or a second transport.

## GAP-FD-001 — verified remote epic-worktree cleanup

**Status:** open; blocks physical-cleanup certification and Phase 0 activation.

**Flowbee-owned decision:** after a concrete merge commit, remove the admitted
epic's private worktree only after the builder session's exact incarnation has
positive absence. Flowbee chooses the action, retry budget, and explicit target.

**Identity already available:** `host_id`, `store_id`,
`tmux_server_instance_id`, `lifecycle_key`, `target_epoch`, `profile_id`,
`workspace_root_id`, `workspace_relative_path`, `lease_id`, and `lease_epoch`.
Raw paths, CWD, tmux names, PIDs, and wall-clock proximity are not acceptable
authority.

**Missing Driver/SDK operation:** a lifecycle operation that removes (or proves
absent) the exact registered workspace target after rechecking the target/lease
epoch and session absence under the same lifecycle fence. It needs a durable,
idempotent receipt addressable by Flowbee `action_id`, with outcomes that
distinguish:

- removed;
- already absent;
- refused because a live session/incarnation still owns it;
- stale target/lease epoch;
- delivering/uncertain after a crash; and
- definite failure.

The receipt must carry the exact workspace identity and the mechanical
absence/removal observation. A submitted request, prose, or a session-stop
receipt alone cannot prove workspace removal.

**Current safe posture:** the v2 effect runtime can verify merge outcome, prove
the parked builder session absent, delete the exact registered GitHub branch,
and release Flowbee delivery reservations. Production activation remains gated
until the remote worktree target has the verified operation above; Flowbee must
not substitute the legacy inferred-path SSH removal.

## GAP-FD-002 — authenticated remote live capacity collection

**Status:** open; local control-plane-host collection is implemented; blocks
capacity-v2 activation when any enabled seat is physically remote.

**Flowbee-owned decision:** periodically collect one live, identity-bound usage
observation for every enabled seat and atomically publish a complete fleet
generation. Flowbee owns the expected host/account/credential-lineage binding,
collection cadence, backoff, generation fence, capacity policy, holds, and
alerts.

**Existing surfaces are insufficient:** the worker API is an outbound
worker-to-control-plane lease/result channel whose usage report is scalar and
self-reported. It cannot authenticate a control-plane-initiated live provider
read on a specific config home. Driver v2.3 authenticates session lifecycle,
observation, and routed terminal messages; it deliberately exposes no arbitrary
provider command or credential/config-home probe operation. A routed agent
message and its receipt cannot establish live billing truth.

**Missing operation:** an authenticated host collector peer, preferably as a
fixed capability on the existing fleet connection (or a future binding Driver
SDK operation), which:

- authenticates immutable `(host_id, collector_id)` from the transport peer;
- accepts a generation id and an allowlisted set of operator-bound seat/config
  identities, never credentials or arbitrary commands;
- performs the provider's fixed live read locally;
- returns one sanitized observation per requested seat with account identity,
  credential-lineage digest, source/trust/integrity, windows, capture time,
  retry metadata, and adapter version;
- fences replay by generation and collector/host incarnation; and
- cannot schedule work, change bindings, or mutate Flowbee workflow state.

**Current safe posture:** `flowbee serve` builds an enrolled local `HostClient`
and runs `FleetService` at startup and periodically. Readiness revalidates every
enabled seat before every generation. An enabled remote seat, an unbound seat,
or a seat whose expected host is not backed by an authenticated client aborts
activation with `GAP-FD-002`; no SSH cache probe, raw tmux operation, routed
agent prompt, or private second protocol is substituted. This preserves the
all-host atomic-generation invariant and fails closed until the missing remote
transport exists.

## GAP-FD-003 — authenticated control-plane message origin

**Status:** implemented by Tmux Driver v2.4; Flowbee activation remains gated on
the exact metadata and authenticated-capability probes below plus Flowbee's live
conformance tests. Do not infer availability from a daemon version, a successful
socket connection, or the presence of a token file.

**Flowbee-owned decision:** Flowbee commits an immutable payload, action epoch,
recipient incarnation, and directional authorization before asking Driver to
insert a control-plane-authored message. The authenticated Flowbee principal is
the actual origin; Flowbee must not attribute that message to an unrelated
Interactor, Orchestrator, builder, reviewer, or operational-agent session.

**Binding activation proof:** Flowbee must first read `GET /v2/meta` and require
`features.control_principal_origin=true`. Because that only proves daemon-wide
protocol support, Flowbee must then use its configured bearer for
`GET /v2/control/capabilities` and require all of the following in the same
response:

- `format_version` is exactly
  `tmux-driver.control-principal-origin-capability/v1`;
- `principal_id` is exactly `flowbee-control`;
- `supported=true` and `authorized=true`; and
- `missing_scopes=[]`.

Any missing, malformed, false, or mismatched value keeps the capability disabled.
Flowbee leaves affected actions in the existing durable, visible
`route_unavailable` hold and makes zero route or message mutations.

**Adopted route and receipt contract:** after Flowbee durably commits the action,
exact payload/hash, epoch, recipient incarnation, and bounds, it creates the
directional grant with `POST /v2/routes/grants`. The request supplies
`sender_principal_id=flowbee-control`, exact `recipient_session_id`, exact
`recipient_pane_instance_id`, grant `epoch`, payload limit, draft policy, and
expiry. It contains no `sender_session_id` or `sender_agent_run_id`. The strict
response format is `tmux-driver.control-route-grant/v1` and must preserve that
same principal, recipient, pane incarnation, epoch, and bounds.

Flowbee sends through the existing `POST /v2/messages` endpoint with the action
ID as the idempotency key, exact grant/recipient/epoch, UTF-8 payload, and payload
SHA-256. A direct-origin send omits `on_behalf_of_session_id`. Its strict receipt
format is `tmux-driver.control-delivery-receipt/v1` and carries
`sender_principal_id=flowbee-control` rather than nullable or fabricated session
sender fields. `submitted` proves terminal insertion only; separate exact Driver
or product facts must prove processing and workflow-stage completion. A
`delivering` crash becomes `uncertain` and is never blindly resent.

Existing session A-to-B routes and the legacy explicitly authorized on-behalf
variant remain separate formats and are unchanged. Flowbee must not use either
variant for its own deterministic control-plane authorship. It must never create
a fake `flowbee-control` `DriverSessionBinding`, substitute a product-agent
session, call raw tmux or `tmux-send`, or introduce a peer transport.

**Credential upgrade:** add the exact `messages:send` scope with Driver's supported
atomic credential operation; never hand-edit the token database:

```bash
td --json auth token-update \
  --config /absolute/path/daemon-tokens.json \
  --principal-id flowbee-control \
  --add-scope messages:send \
  --reload-pid <driver-pid>
```

The command preserves the bearer hash, atomically replaces and fsyncs the
owner-only (`0600`) credential database, and signals Driver with `SIGHUP`. The
operator must re-run the authenticated capability probe after reload; command
success alone does not activate Flowbee.

**Closure evidence still required in Flowbee:** strict wire parsing and fake
contract tests, real UDS metadata/capability probing, one direct-origin
grant/send/receipt replay, route denial, changed-body idempotency conflict,
recipient-incarnation fencing, crash-uncertain no-resend, transport-success versus
stage-success, and zero direct tmux/tmux-send paths. Until these pass against the
current Driver worktree and the pinned candidate, production routed messaging is
not live and the fail-closed hold remains the correct state.

## GAP-FD-004 — atomic managed-session bootstrap artifact injection

**Status:** shipped wire contract; runtime certification pending. The binding
authority is tmux-driver
`docs/design-v2.5-q3-lifecycle-bootstrap-addendum.md`. Flowbee must require all
five exact `/v2/meta` contract entries before emitting this effect:

- `lifecycle_ensure=tmux-driver.lifecycle-ensure/v3`;
- `lifecycle_ensure_bootstrap_artifact=tmux-driver.lifecycle-ensure-bootstrap-artifact/v1`;
- `lifecycle_human_visible_session=tmux-driver.lifecycle-human-visible-session/v1`;
- `lifecycle_managed_display_name=tmux-driver.lifecycle-managed-display-name/v1`; and
- `lifecycle_flowbee_credential_install=tmux-driver.lifecycle-flowbee-credential-install/v1`.

Ensure v3 carries `launch.bootstrap` with exact `artifact_id`,
`format=initial_prompt_utf8/v1`, `payload_sha256`, and `content_utf8`; the
profile injects those public bytes as one pre-exec argv element. It separately
carries the secret-bearing `launch.credential_envelope` with exact envelope ID,
`format=flowbee_target_bearer_utf8/v1`, credential epoch, hash, and secret. The
credential is installed into Driver's owner-only target area and only its path
is exposed through the profile's fixed environment. Public bootstrap and secret
credential are never combined or persisted in Flowbee's public action ledger.

The v3 idempotency fingerprint binds target/action/lease fences, profile,
workspace, display name, bootstrap ID/hash, and credential ID/epoch/hash. The
same action and generation may only rebound the same exact material. Replacing
an incarnation requires a new action plus higher target and credential epochs;
Stop removes Driver's local file, while Flowbee separately revokes issuer-side
authority. Receipts are `tmux-driver.lifecycle-receipt/v3` and expose only
content-free bootstrap/credential status and hashes. Any uncertain staging,
install, or exec outcome is reconciled by the original action; it is never
blindly resent with changed material.

Managed worker presentation names are always
`flowbee-worker-{model}-{project-slug}-{epic-slug}` and are display-only. They
are requested only from a profile with `managed_display_name=true`; stable
Driver identities remain the sole routing and recovery authority.

**Current safe posture:** Flowbee must keep worker creation fail-closed until
the pinned daemon independently passes the v2.5 runtime/conformance gates and
Flowbee's strict request/receipt, replay, uncertainty, replacement, Stop-removal,
and secret-nondisclosure tests. The old post-spawn routed-message contract is
not accepted as Ensure-time bootstrap injection and cannot make this gate green.

**Live certification evidence (2026-07-19):** the installed pinned
`local.tmux-driver.default` daemon was read-ready and authenticated the
`flowbee-control` principal, but Flowbee's read-only UDS conformance stopped at
metadata validation because `/v2/meta` omitted the exact
`lifecycle_profile_inventory="/v2/lifecycle/profiles"` feature entry. This is
not a Flowbee fallback opportunity: it prevents profile/domain validation for
all managed v3 effects. Keep local dispatch paused and record the Driver-side
contract correction plus a fresh dual-endpoint conformance result before
activation. The required `managed_dedicated` launchd endpoint was also not
installed at this probe, so the two-endpoint canary gate remains open.
