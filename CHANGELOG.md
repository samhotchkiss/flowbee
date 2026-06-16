# Changelog

All notable changes to Flowbee are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

This entry summarizes the work to date: the **M0–M12 engine** (the deterministic,
event-sourced control plane, built walking-skeleton-first per `BUILD.md`) and the
**F1–F14 flow-pass** (the layer of flow, provisioning, capacity, and UX work that
followed on top of the engine).

### Engine milestones (M0–M12)

- **M0 — Scaffold.** Buildable single static binary; embedded SQLite
  (`modernc.org/sqlite`, pure-Go, `CGO_ENABLED=0`, single connection) as the store of
  record; embedded-SQL migration runner (idempotent on re-run); in-process
  timer/dispatch loop; stdlib CLI dispatch; health endpoints; CI.
- **M1 — Deterministic control-plane core (the lease thread).** `job_events` ledger +
  `jobs` projection (in-tx fold); atomic claim via partial unique index + `lease_epoch`
  fencing (stale epoch → 409); private worker API (register/lease/heartbeat/result/
  release); stub worker; SSE `/v1/events` + minimal board; concurrent-claim race test
  and replay test (fold == projection).
- **M2 — Ledger spine + job model + scheduler core.** Full event-sourced job record;
  topological walk over `blocked_by`; priority + aging; capability matching;
  `no_eligible_worker` alarm; attempt limits → `needs_human`.
- **M3 — Flow engine + build flow + `code_review` gate.** Flow/role YAML loader with
  §5.6 neutrality lint; pure-function build-flow state machine; deterministic gate that
  mints a SHA-bound, tamper-evident verdict from a `FactSource` (never from a worker's
  self-reported status); code-review bounce loop.
- **M4 — Enforced anti-affinity at lease time.** A worker can never review its own work;
  same-`model_family` workers excluded; holds under race.
- **M5 — Real worker harness + attestation + repo provisioning.** Mode A
  (`flowbee work`, one-shot worktree per lease) and Mode B (`flowbee lease`/`submit`)
  worker modes; handshake + enrolled-identity attestation; git worktrees off a bare
  mirror pushed to epoch refs (workers hold no credentials); roster UI.
- **M6 — GitHub reconcile-IN (read-only).** Batched `BoardSweep` GraphQL on a timer;
  webhook listener (HMAC, delivery dedupe, write-ahead inbox, targeted refetch);
  SHA-monotonic and terminal-SHA guards; `superseded` on new commits; real `FactSource`
  for the gate; identity-budget gauge.
- **M7 — project-OUT outbox + spec flow + ADOPT.** Transactional outbox keyed
  `(job_id, action, head_sha)` with a single serialized sender; spec flow
  (author → review gate → materialize issues, BLAKE3 content hashing, content-hash
  supersession); canonical PR-open trigger; batch-size-1 merge queue; ADOPT of
  pre-existing PRs (imported quiescent); branch-protection assertion.
- **M8 — Liveness MVP.** Per-phase soft deadlines + absolute lease cap (Flowbee the sole
  clock); Rung-4 governor; minimal Rung-2 net-diff-convergence-or-abstain; two-rung kill;
  fast-paths; WARN → CANCEL → REVOKE ladder; partition ≠ stall.
- **M9 — Content-integrity gate (the Branch-B safety boundary).** Path denylist;
  declared-vs-actual blast-radius check; deterministic static checks (applies-clean,
  parse/compile, secret-scan, binary allowlist, size bounds) wired as gate predicates.
- **M10 — Cost metering + ceilings.** `{tokens_in, tokens_out, $}` on heartbeat/result;
  per-job and per-flow rollups; enforced ceilings → escalation with mid-flight cancel;
  unified `needs_human` chokepoint in the UI.
- **M11 — Epoch-namespaced side-effects + compensation.** Epoch-namespaced refs
  end-to-end; `(job, epoch)`-scoped CI gating; `compensate()` (drop dead ref, cancel CI,
  draft-back PR, bump epoch); per-job scoped write-credential class; wires the autonomous-
  merge toggle that makes unattended merge safe.
- **M12 — Hardening + cross-box + transport auth + restart recovery + UI polish.**
  Cross-box `bundle`/`scoped_read` provisioning; mTLS + Tailscale node identity (bearer
  fallback); WAL replication + restart-recovery drill; strangler cutover phases; finished
  board/roster/budget/audit/cost UI.

### Flow-pass milestones (F1–F14)

- **F1 — Lease carries context.** Agent task plumbing so a lease delivers the work
  context to the worker.
- **F2 — Autonomous-merge config (Branch B).** Configurable self-merge posture plus
  content-policy configuration.
- **F3 — Credential-less cross-box provisioning.** The worker returns a patch and
  Flowbee performs all git writes.
- **F4 — Amend-in-place issue review.** `needs_design` handling and epic-level review.
- **F5 — Identity files + configurable flow.** Hire-time identity files, configurable
  flow definitions, and per-step overrides.
- **F6 — Worker capacity.** Per-model slots, accounts, usage tracking, ceilings, and
  weights.
- **F7 — Board lifecycle.** Backlog, `needs_design` endpoint, user-agent loop, and the
  `flowbee` label.
- **F8 — Merge conflicts.** Reservations, a `resolve_conflict` job type, and
  integrated-head re-review.
- **F9 — Multi-repo.** One control plane over a set of repos with a shared fleet.
- **F10 — CI as a pluggable fact.** Pluggable CI fact source plus a test job type.
- **F11 — Issue-archive markdown projection.** Markdown projection of the issue archive.
- **F12 — Web UI productionization.** Fleet and board dashboards moved into
  `internal/web`.
- **F13 — Onboarding.** `flowbee init`, `flowbee doctor`, and onboarding docs.
- **F14 — Doc reconciliation.** Reconciling the docs with the built reality.

[Unreleased]: https://github.com/swh/flowbee/commits/main
