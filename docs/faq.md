# Flowbee FAQ

A quick-start reference for people who are **new to running Flowbee**. It covers the
core mental model, a couple of load-bearing design decisions, and the practical task of
standing up a fleet. For the full runbook see [operating.md](operating.md); for the
architecture see [DESIGN.md](../DESIGN.md).

## What is "reconcile-first"?

Flowbee continuously drives **observed state toward desired state** rather than reacting
once to events. Operations are **level-triggered**: on every reconcile sweep the control
plane re-derives what to do from the current world (GitHub facts + the local ledger),
instead of being **edge-triggered** by a one-shot event it must not miss.

For an operator this has three practical consequences:

- **Actions are idempotent and safe to retry.** Re-running a reconcile that has nothing
  to do is a no-op, so a hiccup never compounds.
- **Recovery is re-derivation, not replay.** A crashed or restarted control plane simply
  reconciles again and rebuilds the world from GitHub + the ledger — there is no
  in-memory state to lose and no log to replay.
- **You drive the system by changing desired state**, not by issuing imperative commands.
  Label an issue, POST a spec, or edit config; the next sweep makes reality match.

See the intro to [operating.md](operating.md) for how this shapes day-to-day operation.

## Why is there no LLM in the control plane?

The control plane must be **deterministic, auditable, and fast**. The same inputs must
always produce the same reconcile decisions, every action must be reproducible and
reviewable after the fact, and nothing on the critical path can introduce the
nondeterminism, latency, or per-call cost of a model invocation. Because the control
plane never calls a model, it can be unit-tested, its history folds deterministically from
its event log, and it never hallucinates a merge or a verdict.

The intelligence lives **at the edges, in the agents** — they author specs, build changes,
and review diffs. An LLM is perfectly legitimate in that authoring/advisory/planning
tooling (including the planner that POSTs work to the front door); it just lives *outside*
the control loop, never inside the decision that merges to `main`.

## How does self-merge gating work?

With self-merge enabled, a change can merge to `main` **autonomously** — but only when the
gates pass. A reviewer's verdict is treated as a *claim*; the gate mints the authoritative
verdict only when it binds to the **reconciled head/base SHA**, required checks/CI are
**green**, the review is **APPROVED** (by an independent, anti-affinity reviewer — never
the builder), there are no conflicts, and the content-integrity check holds. Gating is
what makes an unattended merge safe: every precondition is derived from reconciled facts,
not a worker's say-so.

When a gate blocks a merge, the change does not advance to `merging`; instead it stays in
its current stage or falls back to a human. A common case: pushing to `main` while one of
its issues is in review moves the head SHA, supersedes the SHA-bound verdict, and falls
back to `merge_handoff` — correct safety behavior, not a bug. To inspect why, read the
control plane's transition log and the issue's review findings comment; the
[pipeline stages](pipeline.md) doc explains each gate, and [operating.md §7](operating.md)
covers self-merge specifically.

## How do I bring up a fleet?

At a high level:

1. **Declare what you're running.** Configure the control plane (the repos it serves, the
   lease TTL, GitHub credentials) — see the config and `flowbee.yaml` walkthrough in
   [operating.md](operating.md) and [config.md](config.md).
2. **Start the control plane.** It runs the scheduler, the reconcile-in / project-out
   loops, and the lease API, and owns the single-file SQLite store. The fast path is
   `flowbee up`, which brings up the control plane *and* a worker per role on one box (see
   the [README quickstart](../README.md#quickstart)); multi-box deployments run the
   control plane and worker fleet as separate commands per [operating.md](operating.md).
3. **Let workers join.** Workers are thin pull-loops: each dials **out** to the control
   plane, smoke-tests its agent CLI, leases jobs, and reports back. They commit and push
   with their own box's key and never touch GitHub directly. Add or remove boxes to scale.
4. **Confirm it's healthy and reconciling.** Check fleet health (`GET /v1/fleet-health`):
   `stranded: true` means work is waiting with no live worker — the loud "is the fleet up?"
   signal. Watch the control plane's transition log and the dashboard, and feed it work by
   labeling an issue `flowbee:build` or POSTing to `/v1/specs`.

The exact flags and environment variables are documented in
[operating.md](operating.md) — prefer it over guessing, as command surfaces evolve.
