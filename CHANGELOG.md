# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Early and moving fast. The deterministic engine (milestones **M0–M12**) is built,
committed, and green; a second pass of configurable-flow features (**F1–F14**) layers
the operator surface, multi-repo support, and onboarding on top of it.

### Added — Flowbee v2 control plane (pre-activation)

- Durable epic deliveries and immutable, epoch-fenced Driver action outboxes with
  separate transport receipts and stage evidence.
- Crash-recoverable CI-green-to-review handoff, reviewer-verdict, builder lifecycle,
  merge/cleanup, and delivery backstop reconcilers with pushed control alerts.
- Strict tmux-driver v2.3 observation/lifecycle integration over UDS; no product path
  calls raw tmux or standalone `tmux-send`.
- Authenticated project workspaces, durable human conversations and typed decisions,
  one-time fragment login links, CSRF protection, and an offline owner-only bootstrap.
- Multi-project ownership, portfolio views, fair dispatch, project circuit breakers,
  project-scoped identities, and restart acceptance tests.
- Live Codex/Grok capacity collection and fail-closed identity/lineage-aware routing.

Full control-plane-authored Driver messaging remains held on `GAP-FD-003`: the
shipped Driver requires an exact live session sender and has no authenticated
non-session Flowbee origin. The dashboard exposes this as degraded; synthetic sender
bindings and direct terminal fallbacks are rejected.

### Added — engine milestones (M0–M12)

- **M0 — Scaffold.** Buildable single binary; stdlib CLI dispatch
  (`serve`/`migrate`/`work`/`lease`/`submit`/`seed`); embedded SQLite store
  (`modernc.org/sqlite`, single connection) with an idempotent migration runner; an
  in-process timer/dispatch loop; health + private listeners.
- **M1 — Deterministic control-plane core (the lease thread).** Event-sourced
  `job_events` ledger with an in-transaction fold to the `jobs` projection; the atomic,
  fenced exactly-once claim (partial unique index + `lease_epoch`, stale epoch → 409);
  private worker API (register / lease long-poll / heartbeat / result / release); a stub
  worker; SSE `/v1/events` live board; the concurrent-claim race + replay tests.
- **M2 — Scheduler core.** Full job model; topological walk over `blocked_by`; priority
  with aging; capability matching; `no_eligible_worker` alarms; attempt exhaustion →
  `needs_human`.
- **M3 — Flow engine + build flow + code-review gate.** Flow/role YAML loader with the
  provider-neutrality lint; the pure build-flow state machine; gates that **mint** a
  SHA-bound, tamper-evident verdict from a `FactSource` (never from a worker's
  self-reported status); the code-review bounce loop.
- **M4 — Enforced anti-affinity at lease time.** A worker can never review or merge its
  own work; same-`model_family` exclusion; single-provider-fleet alarm.
- **M5 — Real worker harness + attestation + provisioning.** Mode A (`flowbee work`,
  spawns the agent, one-shot worktree per lease) and Mode B (`flowbee lease`/`submit`
  thin client); enrolled-identity attestation; same-box `git worktree` off a bare mirror
  pushed to an epoch ref with no worker credentials; roster UI.
- **M6 — GitHub reconcile-IN.** Single outbound identity; batched `BoardSweep`; webhook
  listener (HMAC, delivery dedupe, write-ahead inbox); SHA-monotonic and terminal-SHA
  guards; `superseded` re-arming; the real `FactSource` for the M3 gate.
- **M7 — project-OUT outbox + spec flow + ADOPT.** Transactional, serialized outbox;
  canonical PR-open; the spec flow (author → review gate → `materialize_issues`, BLAKE3
  content hashing, content-hash supersession, lens anti-affinity); batch-size-1 merge
  queue; ADOPT of pre-existing PRs (imported quiescent); branch-protection assertion.
- **M8 — Liveness MVP.** Per-phase soft deadlines + an absolute lease cap; the Rung-4
  governor; net-diff-convergence-or-abstain Rung-2 detection; the two-rung kill;
  fast-paths; the WARN → CANCEL → REVOKE ladder.
- **M9 — Content-integrity gate (the Branch-B safety boundary).** Path denylist;
  declared-vs-actual blast-radius checks; deterministic static checks
  (applies-clean@base, parse/compile, secret-scan, binary allowlist, size bounds) wired
  as self-merge predicate conditions.
- **M10 — Cost metering + ceilings.** Per-job and per-flow `{tokens_in, tokens_out, $}`
  rollups; enforced ceilings → `needs_human` + mid-flight `cancel`; the unified
  `needs_human` escalation chokepoint in the UI.
- **M11 — Epoch-namespaced side-effects + compensation.** Epoch-namespaced refs promoted
  only post-validation; `(job, epoch)`-scoped CI gating; `compensate()` (drop dead ref,
  cancel CI, draft-back PR, bump epoch); the resolved §14 self-merge toggle enabling
  unattended merge.
- **M12 — Hardening.** Cross-box `bundle` / `scoped_read` provisioning; mTLS + Tailscale
  node identity (bearer fallback on loopback); WAL replication + KeepAlive + documented
  RPO; strangler cutover; the finished operator UI.

### Added — flow-pass milestones (F1–F14)

- **F1 — Self-contained lease context.** A resolved context block (identity + task /
  spec / acceptance) folded onto the job and carried in the `LeaseGrant`; the default
  agent-cmd convention writes it into the workspace; GitHub issue-body intake.
- **F2 — Operator self-merge posture.** Configurable content-integrity posture for
  autonomous merge (ceilings + an operator-supplied extra denylist).
- **F3 — Credential-less cross-box provisioning.** The `bundle` provisioning path;
  workers hold no GitHub credentials — Flowbee performs all git writes.
- **F4 — Issue review.** Amend-spec-in-place, design-fork parking (`needs_design`),
  human-supplied design decisions, and the epic-level issue-review barrier.
- **F5 — Configurable flows.** Optional/droppable stages, multi-reviewer support,
  override precedence (role < flow < epic < job), and stage→hire-slug assignment fenced
  into the lease.
- **F6 — Worker capacity.** Per-model concurrency slots, named accounts, usage
  reporting, and ceiling-gated account rollover selection.
- **F7 — Backlog board lifecycle.** Seed into `backlog` (tracked but not scheduled),
  deliberate promotion to `spec_authoring`/`ready`, and `flowbee:adopt` direct-to-GitHub
  issue adoption.
- **F8 — Merge conflicts.** Blast-radius reservations and the `resolve_conflict` path
  for both trivial rebases and real overlapping-edit conflicts.
- **F9 — Multi-repo control plane.** A repo registry parsed from YAML; one control plane
  runs a per-repo reconcile-IN + project-OUT with repo-scoped handles.
- **F10 — Pluggable CI fact.** A `test` job type with diff-derived capability
  constraints that produces the `ci_green@sha` (`test_ci_recorded`) fact.
- **F11 — Per-repo history writer.** On merge, a post-merge local-git history write
  (`docs/history/<id>.md`) plus the issue-archive projection.
- **F12 — Productionized operator UI.** The `internal/web` Fleet / Board / Dashboard /
  Roster panes and per-stage detail drawer, rendered live off the store.
- **F13 — Onboarding.** `flowbee init` (scaffold `flowbee.yaml` + flows/) and
  `flowbee doctor` (validate the scaffolded repo), with docs.
- **F14 — Documentation reconciliation.** Docs reconciled to built reality (SQLite as
  the store of record, determinism recorded as an invariant, Branch B resolved, Mode-A
  default + PAT auth).

[Unreleased]: https://github.com/swhme/flowbee/commits/main
