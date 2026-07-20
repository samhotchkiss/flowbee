# Flowbee v2: local one-epic canary

This is the shortest safe path to a live v2 canary: one local project, an
already-running adopted Interactor, one Flowbee-managed Orchestrator, one Codex
builder window, and one admitted epic. It deliberately defers the bare `flowbee`
bootstrap/attach wrapper. The operator starts the plane manually.

This is a **local-single-host** procedure. It must not use a remote seat, SSH
workspace, raw tmux, raw `tmux-send`, legacy `flowbee epic start`, a working-tree
binary, or the production `~/.flowbee/flowbee.db`/listener. A distinct-family
reviewer still must be pre-bound and capacity-ready before admission: v2 will not
admit an epic that cannot be independently reviewed. The first active worker window
is the one Codex builder; the reviewer is reserved for the later review stage.

The complete evidence manifest and extended drill remain in
[the Phase 1 candidate evidence](flowbee-v2-p1-candidate-evidence.md) and
[the full canary runbook](flowbee-v2-phase1-canary.md). This document narrows their
activation sequence; it does not weaken a gate.

## 0. Fix the canary boundary before any mutation

Record these exact values in the canary log. Do not substitute paths by convention.

```bash
export FLOWBEE_CANARY_BINARY=/Users/sam/.flowbee/bin/flowbee-p1-<pinned-commit>
export FLOWBEE_CANARY_BINARY_SHA256=<full-sha256>
export FLOWBEE_CANARY_SOURCE_COMMIT=<full-git-commit-used-to-build-the-binary>
export FLOWBEE_CANARY_CONFIG=/absolute/path/to/canary/flowbee.yaml
export FLOWBEE_CANARY_DB=/absolute/path/to/canary/flowbee.db
export FLOWBEE_CANARY_ENV=/absolute/path/to/canary/serve.env
export FLOWBEE_CANARY_PROJECT=russ-canary
```

The DB must be an explicit canary copy—not `~/.flowbee/flowbee.db`. Confirm that
there is no listener or writer for that canary DB before inventory mutation. The
candidate binary is immutable for this drill:

```bash
shasum -a 256 "$FLOWBEE_CANARY_BINARY"
"$FLOWBEE_CANARY_BINARY" version
```

The hash must equal `FLOWBEE_CANARY_BINARY_SHA256`, and `version --json` must report
the exact `FLOWBEE_CANARY_SOURCE_COMMIT` checked out for the conformance test. A
mismatch is a stop condition; never certify a binary with tests compiled from a
different source revision.

## 1. Certify the live Driver first (read-only)

The Driver gate is first because no Flowbee fallback is allowed. Supply the actual
external/default and managed_dedicated UDS/token pairs. Tokens must be owner-only;
the script refuses symlinks and group/world-readable files.

```bash
export FLOWBEE_DRIVER_EXTERNAL_SOCKET=/exact/external-default/api.sock
export FLOWBEE_DRIVER_EXTERNAL_TOKEN_FILE=/owner-only/external-default.token
export FLOWBEE_DRIVER_MANAGED_SOCKET=/exact/managed-dedicated/api.sock
export FLOWBEE_DRIVER_MANAGED_TOKEN_FILE=/owner-only/managed-dedicated.token

./tools/phase1_canary_preflight.sh
```

Green here proves the pinned artifact identity plus read-only `/v2/meta`, control
principal capability, snapshot, and observation replay against *both* live daemons.
It makes no Driver or Flowbee mutation. It is not enough by itself: before the
serve flip, run the managed-domain isolated lifecycle/control-origin drill from the
full evidence manifest with `FLOWBEE_DRIVER_LIVE_LIFECYCLE=1` and
`FLOWBEE_DRIVER_LIVE_CONTROL_ORIGIN=1`. Never run that mutation drill against the
adopted external/default Interactor endpoint.

Stop if either endpoint is absent, reports another host/store/domain, lacks
`control_principal_origin`, or does not authorize `flowbee-control`. Do not collapse
the two domains into one endpoint or add a default fallback.

## 2. Prepare the local inventory while the canary writer is stopped

Use only exact Driver IDs obtained from the current endpoint snapshot. A tmux name,
raw pane `%N`, CWD, PID, provider text, or time proximity is not an identity.

1. Register the canary project and attach its already-configured repository.
2. Register `interactor` and `orchestrator` actor routes.
3. Commit an **adopt** lifecycle intent for the existing Interactor on
   external/default. It requires the exact watch, session, pane incarnation, and
   agent-run plus the mandatory managed recovery profile
   `claude_interactor_managed`; use `flowbee project actor-lifecycle --help` to
   construct the exact command from the live IDs.
4. Commit an **ensure** lifecycle intent for the Orchestrator on
   managed_dedicated, with exact host/store/domain/server, profile, workspace root,
   workspace-relative path, lifecycle key, and target epoch.
5. Bind one local Codex seat to the managed_dedicated endpoint with
   `flowbee seat bind-driver`. Bind its fresh, authenticated capacity identity with
   `flowbee seat capacity-probe` followed by the printed `bind-capacity` command.
6. Bind one local, fresh, distinct-family reviewer seat and capacity identity. The
   admission gate requires it even though no reviewer window is launched yet.

Use project IDs and Driver inventory values explicitly. The existing examples in
[onboarding](../onboarding.md#5-flowbee-v2-project-and-fleet-inventory) show the
required forms. A direct `bind-session` for an Interactor/Orchestrator is disabled;
their lifecycle intent is the only supported route.

Before proceeding, `flowbee project status --project-id "$FLOWBEE_CANARY_PROJECT"`
must show no static inventory hold. Keep serve stopped for every inventory mutation;
these commands take the writer lock and refuse to race a running plane.

## 3. Snapshot, then manually start the pinned canary

Take and verify a rollback snapshot **before** migrations. This is mandatory even
though the migrations are additive:

```bash
export FLOWBEE_CONFIG="$FLOWBEE_CANARY_CONFIG"
"$FLOWBEE_CANARY_BINARY" backup --keep 10
# Record the returned path, then hash that exact backup file.
shasum -a 256 /exact/path/to/pre-canary.db
```

Copy the prior canary environment file before changing it. Put the v2 flags, exact
dual-endpoint inventory path, local-capacity collector identity, owner-only human and
ingress keys, watchdog project ID, and loopback listener address in
`$FLOWBEE_CANARY_ENV`. Enable only the v2 flags required by the full runbook; do not
add a Matrix/webhook/Slack provider sink. Human notifications are projected only to
the adopted project Interactor through Driver.

Start exactly `FLOWBEE_CANARY_BINARY` with that canary config/environment. Do not run
`go run`, `go build` output, or a workspace executable. Confirm the exact PID,
binary path, canary DB, and loopback listener. Then start the independent watchdog
for the same project and wait for its signed heartbeat to open readiness.

`/healthz` and `flowbee project status` must become green only after all exact actor,
Driver, capacity, and watchdog facts are current. A route/capacity/actor hold is a
stop, not an invitation to hand-edit the DB or send tmux keys.

## 4. Admit exactly one focused epic and observe one worker window

Use the dashboard’s ordinary focused project request (or the existing v2
work-intent API), scoped to `FLOWBEE_CANARY_PROJECT`. Submit one request only and
retain its idempotency key/version. Do **not** invoke legacy `flowbee epic start`:
v2 disables it specifically to avoid a second lifecycle/terminal-control path.

Observe these durable facts in order:

1. Conversation/work-intent and admitted epic exist before agent acceptance.
2. Exactly one Builder lifecycle intent/action exists for the local Codex seat.
3. Driver Ensure produces one current stable identity; the board shows the worker
   window and its associated epic.
4. A Driver receipt proves terminal insertion only. It is not build acceptance or
   stage completion; wait for separate observed progress/build evidence.

Stop immediately for duplicate lifecycle actions, an identity/domain mismatch, a
direct tmux action, an inter-session message outside Driver, a non-terminal state with
neither a next action nor a visible hold, or an attempt to use a remote seat.

## 5. Minimal rollback

For a normal rollback, pause dispatch, stop the exact candidate PID, restore the
saved prior canary environment and pinned binary, and start it under the writer lock.
Leave additive data, actions, receipts, and Driver archive intact; a v2 action must
never be rewritten as a legacy on-behalf/direct-tmux send.

For a data restore, stop **every** canary writer first, then use the pinned binary’s
`restore` command against the recorded pre-canary snapshot. Restore is an emergency
recovery mechanism, never a way to erase a hold. Verify the restored canary with
`doctor --offline`, `/configz`, and an authenticated dashboard smoke.

Record the exact artifact hash, Driver endpoint inventory, backup hash, adopted
Interactor stable tuple, Orchestrator lifecycle receipt, seat identities, epic ID,
worker lifecycle action/receipt, and final GO/NO-GO in the candidate evidence file.
