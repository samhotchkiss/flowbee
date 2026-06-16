# Flowbee — build plan

> Executable build plan derived from `DESIGN.md` (the authoritative ground-up design). This is a
> **from-scratch, walking-skeleton-first** build. The design's §13 strangler over the `russ` scripts is
> the *deployment/cutover* story (lands at the end, M12); it is **not** how we build. We build the
> thinnest end-to-end thread that proves the architecture first, then layer GitHub, flows, real workers,
> liveness, and the UI.
>
> Where `DESIGN.md` left a fork, this plan **picks one and flags it** (see §7). The design doc may carry
> minor residual inconsistencies from assembly; the established design facts in the task brief are
> authoritative over the doc where they conflict (notably: the event-sourced ledger as the spine).
>
> ---
>
> ## ⚑ Reconciliation banner (built reality, 2026-06-16) — read before the historical plan below
>
> The engine (M0–M12) is **built, committed, and green**. This plan was written *before* the build and names
> a few substrates that were **changed during construction**. The corrections, authoritative everywhere below:
>
> 1. **Store = embedded SQLite, NOT Postgres/River.** `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`) via
>    stdlib `database/sql` with `SetMaxOpenConns(1)` is the **single store of record**. **There is no
>    Postgres, no River, no pgx, no testcontainers, no docker-compose.** River's cadence/timer/retry/dedup
>    role is served by a **hand-rolled `timers` table + one in-process polling goroutine** (epoch-guarded).
>    SQLite dialect: `?` placeholders; no `TIMESTAMPTZ` (TEXT/RFC3339 + `datetime('now')`); `INTEGER PRIMARY
>    KEY AUTOINCREMENT`; partial unique indexes + `UPDATE … RETURNING` (the atomic claim). Where the plan
>    below says "Postgres", "River", "pgx", "pgxpool", "TIMESTAMPTZ", "testcontainers", or "docker-compose"
>    as the substrate, **read SQLite + the in-process timer loop**; the semantics are preserved, only the
>    dependency changed. §3.2 is the corrected, canonical library table.
> 2. **Determinism is an explicit invariant (I-0).** `internal/{engine,job,ledger,lease,scheduler,flow}` is a
>    deterministic, replayable function of persisted facts — no clock/`math.rand`/`crypto.rand`/ULID/GitHub/LLM
>    imports (the `crypto/sha256` deterministic-hash exception aside); `tools/archcheck` enforces it and stays
>    green. (§1.2, DESIGN §3.6 I-0.)
> 3. **THE ONE DECISION (§14) is RESOLVED: Branch B — autonomous merge, no human gate.** `Policy.AllowSelfMerge`
>    (`config.AllowSelfMerge`, env `FLOWBEE_ALLOW_SELF_MERGE`, `flowbee.yaml: allow_self_merge`) is the
>    **configurable** toggle; production posture `true`. It defaults `false` in code as a fail-safe. The safety
>    net is deterministic — content-integrity gate (I-11) + epoch side-effects (I-12) + reconciled SHA-bound
>    verdict (I-9) + CI-green-at-head (I-5) — not a human.
> 4. **GitHub auth: a fine-grained repo-scoped PAT for the single-operator default**; a GitHub App only at
>    org/multi-repo scale (DESIGN §8.3). **Mode A (`claude -p` / `codex exec`, subscription-covered, harness-
>    driven) is the default** worker mode; Mode B is optional. **Workers hold NO GitHub creds** — Flowbee does
>    all git writes (F3).

---

## 1. Build philosophy

Three commitments govern every milestone. They are testable, not aspirational.

### 1.1 Walking skeleton first

The thinnest end-to-end thread that proves the architecture — **control plane + store + the fenced lease
protocol + a stub worker that leases a manually-seeded job and returns a result + a minimal live view** —
ships before any GitHub, flow logic, or real agent exists (M0–M1). This front-loads the two hardest
correctness claims (deterministic replayable core; fenced exactly-once leasing under concurrency) so they
are *proven before* the complexity that would otherwise hide a bug in them. Only after that thread is green
do we layer: real state machine → DAG/scheduler → flows + gates → anti-affinity → real workers →
GitHub reconcile-IN → project-OUT + spec flow + ADOPT → liveness → content-integrity → cost → epoch
side-effects → hardening.

### 1.2 Deterministic core (the load-bearing architectural commitment)

`DESIGN.md`'s hardest invariant: **flowbee-core (control plane AND worker harness) contains NO LLM and is a
deterministic function of persisted facts** — replayable. We realize this as a strict layering:

```
persisted facts ──fold──▶ EngineState ──engine.Decide()──▶ Decision{ transitions, side-effects, timers }
(job_events,              (immutable                        (DATA only; NO I/O, NO clock read,
 reconciled GitHub        snapshot)                          NO randomness — all injected)
 facts, leases, verdicts)
```

- `internal/engine` is **pure**: it takes an immutable `EngineState` (folded from the ledger + reconciled
  Domain-B facts) plus one triggering `Event`, and returns a `Decision` — a *description* of what should
  happen. It performs zero I/O, reads no clock, generates no IDs. An outer runtime applies the `Decision`
  transactionally. This is what makes replay possible: same event log → same `Decision` stream, byte-for-byte.
- **A worker's `status: succeeded` is a *claim*, never a verdict** (I-9). The only way an LLM result enters
  the core is as a persisted `result_accepted` / `verdict_claim` event; gate logic derives the real verdict
  from reconciled facts. We enforce this mechanically: a CI lint (`tools/archcheck`) forbids the core
  packages from importing `time` (beyond the type), `math/rand`, `crypto/rand`, any ULID minter, any clock,
  any LLM/agent/GitHub package.

### 1.3 Test-first on the lease

The single most important test in the codebase is the **concurrent-claim race test** (M1): N goroutines
race for one `ready` job; **exactly one** wins; the partial unique index is the structural backstop. It is
written under `-race -count=50` against a **real temp-file SQLite DB** (mocks prove nothing; the
single-connection serialization + partial unique index are what the test exercises). Every milestone
ships with a `DONE WHEN` acceptance test that is an observable behavior, runnable in CI. The replay test
(fold == projection) and the stale-epoch fencing test (`409`) are written in M1 alongside it.

---

## 2. Milestone roadmap

Each milestone is independently shippable. `DONE WHEN` is the acceptance gate. The §14 decision is now
**RESOLVED — Branch B (autonomous merge, no human gate)**; during the build it was kept a **policy flip**
(`Policy.AllowSelfMerge` — `config.AllowSelfMerge`, env `FLOWBEE_ALLOW_SELF_MERGE`), never a rewire, so the
resolution is a config setting (production `true`; code default `false` as a fail-safe). I-11 (content-
integrity, M9) and I-12 (epoch-namespaced side-effects, M11) are the milestones that make Branch B safe; both
are built, so the toggle is real.

| M | Goal | Scope (in) | DONE WHEN | DESIGN refs | Deps |
|---|---|---|---|---|---|
| **M0** | **Scaffold.** Buildable single binary, store + timer loop wired, migrations run on boot, health green. | cobra-free stdlib CLI dispatch (`serve`/`migrate`/`work`/`lease`/`submit`/`seed`); SQLite (`database/sql`, 1 conn); embedded-SQL migration runner; in-process timer/dispatch loop (no workers); config; CI (build, vet, lint, smoke). *(as-built: SQLite, not Postgres/River)* | `flowbee serve` boots against an empty SQLite file, runs all migrations idempotently (re-run is a no-op), starts the timer loop, opens health (`/healthz` 200) + private listeners; tests green in CI. | §4, §12.1, §12.3, §12.4 | — |
| **M1** | **Deterministic control-plane core — THE lease thread.** Prove core + fenced exactly-once lease + stub worker + live view against a manually-seeded job. Zero GitHub, zero flow. | `job_events` ledger + `jobs` projection (in-tx fold); §6.3.1 atomic claim + partial unique index + `lease_epoch`; private worker API (register/lease long-poll/heartbeat/result/release), fenced → 409; trivial state machine `ready→leased→building→review_pending`; stub worker; `seed-job`; SSE `/v1/events` + minimal HTML board; replay test. | (1) two stub workers race one seeded job → **exactly one** wins, loser gets 204; (2) stale-`lease_epoch` heartbeat → 409, current → continue; (3) result → `review_pending`, duplicate idempotency-key → identical response, no re-apply; (4) `Fold(events)` == `jobs` projection; (5) lifecycle visible live via SSE. | §6.3 (I-4), §7.1–7.2, §3.5/§6.4, DETERMINISM, §12.1 | M0 |
| **M2** | **Event-sourced ledger as spine + real job model + scheduler core (DAG/priority/aging).** | full §6.1.1 record; topo walk over `blocked_by`; priority + aging; capability match on **claimed-as-attested** (real attest = M5); `no_eligible_worker` timer alarm; `attempts`/`max_attempts` → `needs_human` (partial). | hand-seeded `A→B→C` DAG: `B` blocked until `A` done; aged low-prio offered before high-prio newcomer; worker lacking a required cap never wins, `no_eligible_worker` fires after timeout; run reconstructable by replaying `job_events`. | §6.1, §6.6 (I-6), §5.6 (partial), EVENT-SOURCED | M1 |
| **M3** | **Flow engine + build flow + code_review GATE (deterministic gates, GitHub stubbed).** | flow/role YAML loader + §5.6 neutrality lint; full §6.2 build-flow state machine (pure fn); gate **mints** verdict from a stubbed `FactSource` (I-9 shape); code-review bounce loop (`bounces`/`max_bounces`); `self_merge`/`handoff` toggle (default handoff). | seeded build job drives `building→review_pending→code_review` across distinct builder+reviewer stubs; `changes_requested` bounces + increments `bounces`; `max_bounces`→`needs_human`; `approved` + green stub FactSource mints a SHA-bound tamper-evident verdict (never from worker `status`) → `mergeable`→`merge_handoff`; the §5.6 lint **fails the build** on a planted provider literal. | §5.1–5.4, §5.6, §6.2, §6.2.3 (I-6), §5.5 (partial, I-9) | M2 |
| **M4** | **Enforced anti-affinity at lease time (I-10).** | full §6.3.1 `NOT EXISTS` clauses wired to real sibling lineage; sibling-job edges populated; single-provider-fleet alarm. | a worker that built `J` never wins `J`'s `code_review` lease (0 rows); same-`model_family` worker excluded; with only `model:codex` workers, review stage raises `no_eligible_worker`; exclusion holds under race. | §5.5 (I-10), §6.3.1, §9.3 | M3 |
| **M5** | **Real worker harness + attestation + repo provisioning + BOTH worker modes.** | Mode A `flowbee work` (spawns `FLOWBEE_AGENT_CMD`, one-shot worktree per lease, `collect_work_product`); Mode B `flowbee lease`/`submit` (thin client / MCP shim); attestation (handshake arch/os + enrolled-identity allowlist for `role`/`model_family`/`tool`); same-box `git worktree` off bare mirror, push to epoch ref, no creds; roster UI. | `flowbee work` with a fake agent leases a build job, provisions a worktree at `base_sha`, pushes `refs/flowbee/<job>/epoch-<n>`, submits a real patch; a Mode-B session completes the same kind via `lease`/`submit`; unattested cap never matched; roster shows both workers + stale-hb badge. | §7.1–7.6, §9.4.1, §12.6.2 | M3, M4 |
| **M6** | **GitHub reconcile-IN + inbox + App identity (the IN half, read-only).** | single outbound GitHub identity (PAT for single-operator; App at org-scale); batched `BoardSweep` GraphQL on a timer + boot + gap; webhook listener (HMAC, `X-GitHub-Delivery` dedupe, write-ahead inbox, targeted refetch); SHA-monotonic + terminal-SHA guards (I-3); `superseded` (I-5); real `FactSource` for M3's gate; identity-budget gauge. | sweep populates Domain-B columns to match a test repo; forged/replayed webhook rejected/deduped, at worst triggers a refetch of real state (cannot fast-track); new commit to an open PR → job `superseded` + re-armed; budget gauge live; a test asserts reconcile-IN never writes a Domain-A field. | §3.3, §8.1 (I-1/I-2/I-3), §8.3 (I-14), §6.2.4 (I-5), §12.6.3 | M3, M5 |
| **M7** | **project-OUT outbox + spec flow + ADOPT mode (the OUT half; first full chat→merge thread).** | transactional outbox keyed `(job_id, action, head_sha)`, single serialized sender, ≤1 in-flight, `Retry-After`; canonical PR-open trigger (§7.3); spec flow (`spec_author`→`spec_review` GATE→`materialize_issues`, BLAKE3 `spec_content_hash`, content-hash supersession, lens anti-affinity, chat as lineage root); batch-size-1 merge via merge queue; ADOPT mode (I-16); branch-protection assertion (I-8). | against a test repo: spec doc → spec job, commits `spec.md`, hashes, gates on distinct-lens reviewer; edit supersedes prior sign-off; sign-off **materializes a real issue**; builder patch → **Flowbee opens the PR and stamps #**; reviewer approves → `handoff`/`needs_human`; human merges → reconcile-IN flips to `done`; pre-existing PRs imported **quiescent, untouched**; every GitHub action appears once in the audit log keyed `(job_id, action, head_sha)`. | §8.2 (I-7), §8.5.2 (I-5), §11 (I-9/I-10), §12.7 (I-16), §9.6 (I-8) | M6, M5, M3/M4 |
| **M8** | **Liveness MVP (Rung-3 + Rung-4 + minimal Rung-2 + two fast-paths + two-rung kill, I-13).** | per-phase soft deadline + absolute lease cap (the in-process timer loop, Flowbee sole clock); Rung-4 governor (`stall_revocations` → `needs_human`, fleet rate-limit); minimal Rung-2 (net-diff-convergence-or-abstain on the sweep + CI-tolerance + circuit breaker; spec-flow forces abstain); two-rung kill (I-13); two free fast-paths; partition≠stall; WARN→CANCEL→REVOKE ladder. | a forever-heartbeating no-net-diff worker killed **only** when Rung-2 + Rung-3 agree, **not** when Rung-2 abstains; past absolute cap → unilateral revoke; partitioned worker's healed-link result 409'd; exited agent fast-pathed to `failed`; killed-and-resumed past governor ceiling sticks in `needs_human`; a healthy 40-min E2E with a CI `running` transition is **not** killed. | §10 (MVP cut), I-13, §6.8, §6.7 (partial) | M5, M6 |
| **M9** | **Content-integrity gate (I-11) — the Branch-B safety boundary.** | path denylist (`.github/workflows/**`, lockfiles+lifecycle, Dockerfiles, secrets, Flowbee's own source + the denylist itself); declared-vs-actual blast-radius; deterministic static checks (applies-clean@base, parse/compile, secret-scan, binary allowlist, size bounds); wired as §5.4 predicate conditions 2–4. | a `.github/workflows/ci.yml` patch forced to `handoff` regardless of `self_merge` request; blast-radius-exceeding patch flagged as tamper → `handoff`; non-applying / secret-tripping patch fails static checks, never self-merge-eligible; with §14 toggle off, clean diffs unchanged (human still merges) — proving the gate is a pure policy-promotion. | §9.2 (I-11), §5.4 (cond. 2–4) | M7, M8 |
| **M10** | **Cost metering + ceilings (I-15) + full escalation chokepoint.** | `{tokens_in, tokens_out, $}` on heartbeat/result, per-job + per-flow rollup; enforced ceilings → `needs_human` + mid-flight `cancel` directive + `flowbee:over-budget`; unified `needs_human` chokepoint surfaced in UI. | a job crossing its ceiling is escalated (live `cancel` + `over-budget` label); per-flow rollup answers "what did this feature cost across spec+build+review"; UI `needs_human` lane shows jobs from all four triggers (attempts, bounces, cost, stall). | §6.7 (I-6+I-15), §12.6.5, I-15 | M5, M8 |
| **M11** | **Epoch-namespaced side-effects + compensation (I-12) — enables unattended merge.** | epoch-namespaced refs end-to-end (promote only post-validation; stale ref orphaned); `(job,epoch)`-scoped CI gating; `compensate()` (drop dead ref, cancel CI, draft-back PR, bump epoch); per-job scoped write credential class; wire the §14 toggle (I-9+I-11+I-12 present → `self_merge` becomes config-enabled). | a worker revoked mid-build then reconnects and pushes to its stale epoch ref → ref never fast-forwarded, its CI can't satisfy the live gate, compensation dropped it + bumped epoch (409'd); live re-dispatch completes; with toggle ON on a clean/denylist-clear/in-budget/unmoved-SHA diff, Flowbee **merges unattended** via the queue with reconciled provenance — denylist/SHA-moved diff still falls to `handoff`; toggle off restores Branch A. | §3.5/§6.5 (I-12), §9.4, §5.4 cond. 6, §14 | M9, M8, M7 |
| **M12** | **Hardening, cross-box provisioning, mTLS, strangler cutover, UI polish.** | cross-box `bundle` + `scoped_read` provisioning (R5); mTLS + Tailscale node identity (bearer fallback on loopback); WAL replication + launchd KeepAlive + documented RPO (kill/restart drill); strangler Phases 0–2 (dark-launch vs `russ` `state.json`, scripts read mirror, Flowbee schedules) + the §13.4 interim rate-limit patch (scripts-only, can precede everything); finished UI (board/roster/budget/audit/cost, SSE). | a second-box worker over Tailscale (mTLS, no installation token) completes a real build; kill+restart `flowbee serve` reconstructs Domain-A from PG + re-reconciles Domain-B with zero job loss (RPO met); dashboard live; strangler Phase-1 collapses N redundant `statusCheckRollup` polls to one sweep with a flat `rateLimit.remaining` floor. | §7.4, §9.5, §12.4, §13, §12.6 | M7, M11 (most parallelizable after M7) |

**Risk front-loading:** the two hardest correctness claims (deterministic replayable core; fenced
exactly-once leasing under concurrency) are both proven in **M1**, before any GitHub or flow complexity.
The two-domain reconciliation (the design's spine) is proven by **M6+M7**. The two Branch-B invariants
(I-11, I-12) are deliberately last (M9, M11) because the human gate (Branch A) makes them deferrable per §14.

**Deferred to v1.1+ (explicit non-goals):** HA/leader-election, multi-repo, multi-tenant, merge-queue
batch-size>1 + `flowbee/review-valid@SHA` re-eval, multi-reviewer fan-out/quorum, full Rung-0/Rung-1 +
adaptive priors, egress confinement (§9.7), stacked-PR depth, a future durable-workflow-engine swap behind the clean interface (§12.5). *(Note: SQLite is the shipped store of record, not a deferred item — the pre-build "SQLite store" deferral is obsolete.)*

---

## 3. Go project architecture

### 3.1 Toolchain & module

- **Module:** `github.com/swh/flowbee` (single Go module).
- **Go 1.25** (installed: `go1.25.6`). Single static binary, `CGO_ENABLED=0`. One embedded SQLite file (`modernc.org/sqlite`, pure-Go) — **no external database server**.
- **One `main` (`cmd/flowbee`), two roles.** "Two binaries / one artifact" = the same binary as control
  plane (`flowbee serve`) vs. worker (`flowbee work` / `lease` / `submit`). An **architecture test**
  (`go list -deps`) asserts the worker subcommand packages never transitively import
  `internal/github`, `internal/engine`, or `internal/store` — making R4 ("workers never call GitHub") a
  **compile-time** guarantee, not a convention.

### 3.2 Library choices (locked, with justification)

> **Reconciled to the built engine (2026-06-16).** The pre-build plan named **Postgres + River + pgx**. The shipped engine uses **embedded SQLite (`modernc.org/sqlite`, pure-Go) as the single store of record** via stdlib `database/sql` with `SetMaxOpenConns(1)`, and a **hand-rolled `timers` table + one polling goroutine** in place of River. There is **no Postgres, no River, no pgx** dependency. The table below is corrected; the rows that named the dropped deps are struck and replaced.

| Concern | Choice | Why this one |
|---|---|---|
| Store / DB driver | `modernc.org/sqlite` (pure-Go SQLite) via stdlib `database/sql`, **single connection** (`SetMaxOpenConns(1)`) | The system is single-operator, single-repo, single-writer. Pure-Go keeps `CGO_ENABLED=0` and a single static binary; one connection serializes writes so the §6.3.1 partial unique index holds without MVCC. SQLite dialect: `?` placeholders, no `TIMESTAMPTZ` (TEXT/RFC3339 + `datetime('now')`), `INTEGER PRIMARY KEY AUTOINCREMENT`, partial unique indexes + `UPDATE … RETURNING` for the atomic claim. **(Replaces the planned pgx/v5 + Postgres — dropped.)** |
| Job queue / timers | **hand-rolled `timers` table** (`due_at`, `expected_epoch`, `fired`) + **one in-process polling goroutine** | Buys what §12.3 needs — transactional enqueue coupled to the state change, retries/backoff, timers (reconcile cadence, soft deadlines, alarms), dedup, sweeper — without an external server. Epoch-guarded deadline checks are idempotent (stale epoch → no-op). **Agent leases are custom on top — never plain queued rows. (Replaces the planned riverqueue/river + riverpgxv5 — dropped.)** |
| Migrations | embedded `//go:embed migrations/*.sql` + a tiny in-process runner (`schema_migrations` table), raw SQLite SQL | One binary owns schema. No goose/atlas/golang-migrate CLI dependency, no River migrator; raw SQL keeps the §6.3.1 partial index legible. Idempotent re-run. |
| HTTP router | stdlib `net/http` 1.22 `ServeMux` (method+path patterns: `POST /v1/jobs/{job}/heartbeat`) | Covers the whole §7.2 surface + webhooks in ~6 routes. Two separate `*http.Server`s (public webhook, private worker API). No chi/framework tax. |
| CLI | stdlib `flag` + a small dispatch in `main.go` (no cobra) | The subcommand set is small and fixed; cobra is dead weight for the skeleton. (If the tree grows past M5, revisit cobra — contained to `cmd/`.) |
| Config | env + a single `flowbee.yaml` parsed with `gopkg.in/yaml.v3` (flows/roles/lenses are YAML from M3) | One YAML parser for config and flows; no viper/koanf global-state. Env overrides via `FLOWBEE_*`. |
| Content hash | `github.com/zeebo/blake3` | §11.5 mandates BLAKE3 for `spec_content_hash`. (Helper present from M1; spec flow uses it in M7.) |
| IDs | `github.com/oklog/ulid/v2` (monotonic) | §6.1.1: Flowbee-minted, time-sortable ULIDs. |
| GitHub | `github.com/shurcooL/githubv4` (GraphQL `BoardSweep`) + `github.com/google/go-github/v66` (REST writes) + `github.com/bradleyfalzon/ghinstallation/v2` (App installation token, I-14) | GraphQL for the one batched read; REST for writes; ghinstallation mints/rotates the single installation token. `git` via shelling to the `git` binary against a local bare mirror. |
| SSE | stdlib `http.Flusher` + `text/event-stream` | Read-only `/v1/events` feed. No dep. (Pulled forward to M1 — see §7 flag 1.) |
| Web UI | Go `html/template` + `//go:embed` + htmx (no SPA build step) | §12.6 is board/roster/feed/cost read straight off SQL. htmx polling (then SSE) beats a JS toolchain inside a single-binary ethos. |
| Auth | v1: signed per-worker bearer tokens (HMAC, enrolled-identity allowlist); mTLS in M12 | §7.6 allows either. Tokens are curl-debuggable over Tailscale and simple to enroll; one `auth.Authenticator` interface contains the swap. |
| Logging | stdlib `log/slog` (JSON handler) | No telemetry pipeline in v1; the auditable log is a DB table, not logs. |
| Testing | stdlib `testing` + `github.com/stretchr/testify/require` + `go.uber.org/goleak` | Replay/race/acceptance tests run against a **real temp-file SQLite DB** (no testcontainers, no Postgres, no docker). The partial unique index + single-connection serialization give the exactly-once-lease guarantee under `-race`; hermetic and fast with zero external services. **(Replaces the planned testcontainers-go/postgres — dropped.)** |

### 3.3 Repo layout

```
flowbee/
├── go.mod  go.sum
├── Makefile
│                                       # (no docker-compose — the store is an embedded SQLite file)
├── .golangci.yml
├── flowbee.yaml                        # sample config
├── DESIGN.md  BUILD.md
├── cmd/flowbee/
│   ├── main.go                         # flag dispatch: serve|migrate|work|lease|submit|seed|version
│   ├── serve.go                        # control plane — the ONLY GitHub caller
│   ├── work.go                         # Mode A: harness-driven worker (spawns agent CLI)
│   ├── lease.go                        # Mode B: thin client, one lease to stdout
│   ├── submit.go                       # Mode B: post result/heartbeat/release from CLI
│   ├── seed.go                         # seed a ready job by hand (the M1 manual seed)
│   └── migrate.go                      # migrate up|down|status
├── internal/
│   ├── config/                         # typed config; env + flowbee.yaml; flows/roles loader (M3)
│   ├── clock/                          # Clock interface (Flowbee is the SOLE clock, §6.3.3) — injectable
│   ├── ulid/                           # ULID minting wrapper (monotonic, testable)
│   ├── store/                          # SQLite (database/sql, 1 conn), Store/Queries, hand-written SQL, migration runner, timers
│   │   └── migrations/*.sql            # embedded
│   ├── ledger/                         # EVENT-SOURCED spine: job_events append + Fold → jobs projection
│   ├── job/                            # job domain types, State/Kind/Role/Stage enums, pure Transition()
│   ├── engine/                         # the deterministic CORE: pure Decide(state,event)→Decision + runtime
│   ├── lease/                          # custom fenced lease: atomic claim, epoch, TTL, heartbeat
│   ├── scheduler/                      # topo walk + priority + aging + capability match + anti-affinity
│   ├── flow/                           # flows/roles/lenses YAML model; neutrality lint; anti-affinity terms
│   ├── worker/                         # registry (enroll/attest) + server-side protocol logic + stub
│   ├── api/                            # the two HTTP servers; /v1 worker API; /webhooks; /v1/events SSE
│   ├── auth/                           # signed worker tokens, enrolled-identity allowlist
│   ├── github/                         # go-github + githubv4 + ghinstallation; single installation identity
│   ├── reconcile/                      # reconcile-IN: BoardSweep, drift, SHA-monotonic + terminal guards
│   ├── project/                        # project-OUT: outbox, (job,action,head_sha) dedupe, Retry-After
│   ├── webhook/                        # HMAC verify, X-GitHub-Delivery dedupe, write-ahead inbox
│   ├── content/                        # content-integrity gate: denylist + blast-radius + static (I-11)
│   ├── gitops/                         # mirror, worktrees, epoch-ref promotion, spec commits, bundles
│   ├── liveness/                       # rung ladder, two-rung kill, governor — driven by the in-process timer loop
│   ├── cost/                           # per-job/per-flow token-$ meter + ceiling (I-15)
│   ├── projector/                      # docs/history/<id>.md + TOC (post-merge read-model)
│   └── web/                            # embedded dashboard (templates + htmx + SSE)
├── client/                             # Mode-B reusable Go client (also the MCP shim surface)
├── flows/                             # default flows.yaml, roles.yaml, lenses/*.md (from M3)
├── tools/
│   ├── archcheck/                      # forbidden-import lint (no clock/rand/LLM/GitHub in core)
│   └── providerlint/                   # §5.6: no provider literal outside model_family:* / lens.prompt_ref
└── test/
    ├── replay/                         # determinism: golden event logs → folded state
    └── acceptance/                     # walking-skeleton + flow integration tests (real temp-file SQLite)
```

### 3.4 Key package interfaces

**The deterministic core** (`internal/engine`) — pure, no I/O:

```go
// EngineState is an immutable snapshot folded from persisted facts ONLY. The core reads nothing else.
type EngineState struct {
    Job        job.Job                  // projection folded from job_events
    Lineage    job.Lineage              // chat→spec→issue→PR edges
    Sibling    map[job.Role]job.Bound   // bound identity/family/lens of sibling stages (anti-affinity input)
    GitHub     reconcile.DomainBFacts   // reconciled-IN facts ONLY: PR#, head/base SHA, CI rollup, merged
    Lease      *lease.Lease             // nil unless active
    Counters   job.Counters             // attempts, bounces, stall_revocations, cost
    Policy     Policy                   // THE ONE DECISION lives here: AllowSelfMerge, denylist, ceilings
    Now        time.Time                // injected clock reading — passed IN, never read by the core
    ContentChk *content.Result          // result of the deterministic gate, if already computed
}

type Event interface{ isEngineEvent() }
type WorkResult       struct { Epoch int; WorkProduct worker.WorkProduct } // a fenced CLAIM, not a verdict
type Heartbeat        struct { Epoch int; Obs worker.Observations }
type ReconcileObserved struct { Facts reconcile.DomainBFacts }
type LeaseClaimed     struct { Lease lease.Lease }
type TimerFired       struct { Kind TimerKind }
type LivenessVerdict  struct { Rungs liveness.RungSet }

// Decision is DATA describing intent. The runtime applies it; the core never acts.
type Decision struct {
    Transitions []job.Transition    // state moves + ledger events to append
    Verdicts    []job.VerdictMint   // tamper-evident sign-offs to mint (post-reconcile, I-9)
    SideEffects []SideEffect         // project-OUT outbox rows: open PR, label, check, comment, enqueue-merge
    GitOps      []GitOp              // promote epoch ref, commit spec, drop ref, compensation
    Timers      []TimerRequest       // timers to (re)arm (the `timers` table): phase deadline, absolute cap, alarm
    LeaseOps    []LeaseOp            // epoch bump, revoke, grant — DESCRIBED here, executed by runtime
    Directive   *worker.Directive    // continue|cancel reply for a heartbeat
    Reject      *RejectReason        // e.g. stale-epoch → 409
}

// Decide is THE deterministic function. Pure: same (state, event) → same Decision, always.
func Decide(s EngineState, e Event) Decision
```

**The pure state machine** (`internal/job`):

```go
type State string  // §6.2.1 catalogue: spec_authoring, spec_review, ready, leased, building,
                   // review_pending, code_review, mergeable, merging, merge_handoff, done,
                   // blocked, needs_human, superseded, cancelled
type Role  string  // spec_author | spec_reviewer | eng_worker | code_reviewer | merger
type Kind  string  // spec | build

// Next is the pure §6.2 state machine. (Job, Trigger) → (Job, emitted events). No side effects.
func Next(j Job, t Trigger) (Job, []ledger.Event, error)

// SelfMergeEligible is the SINGLE canonical §5.4 predicate, evaluated by the core (never the worker).
func SelfMergeEligible(j Job, gh reconcile.DomainBFacts, chk content.Result, p Policy) bool
```

**The event-sourced ledger** (`internal/ledger`):

```go
type EventKind string  // job_created, lease_claimed, worker_started, heartbeat, result_accepted,
                       // verdict_minted, facts_reconciled, lease_released, lease_expired, state_changed, ...

// Append writes one event WITHIN the caller's tx, deriving job_seq = prev+1. MUST share the tx that
// mutates the jobs projection — append + fold are atomic.
func Append(ctx context.Context, tx *sql.Tx, e Event) (int64, error)  // stdlib database/sql, SQLite

// Fold replays events into a projection. PURE: no clock, no RNG, no I/O. Fold(events) == jobs row.
func Fold(events []Event) (job.Job, error)
```

**The store boundary** (`internal/store`) — the only I/O seam the core touches:

```go
type Store interface {
    Tx(ctx context.Context, fn func(q Queries) error) error  // one SQLite tx; outbox/timer enqueue inside fn shares it
    Queries
}
type Queries interface {
    ClaimJob(ctx context.Context, p ClaimParams) (*job.Job, error)   // §6.3.1 atomic UPDATE … RETURNING
    AppendEvents(ctx context.Context, jobID ulid.ULID, evs []ledger.Event) error
    LoadEngineState(ctx context.Context, jobID ulid.ULID) (engine.EngineState, error)
    UpsertDomainBFacts(ctx, prNumber int, f reconcile.DomainBFacts) error  // reconcile-IN ONLY
    EnqueueOutbox(ctx, row project.OutboxRow) error                       // (job,action,head_sha) keyed
    UpsertWorker(ctx, w worker.Registration) (worker.Worker, error)
    BoardSnapshot(ctx) ([]web.Lane, error)
    AuditAppend(ctx, a project.AuditRow) error
}
```

**The custom fenced lease** (`internal/lease`) — *not* a timer/queue-worked row:

```go
type Lease struct {
    LeaseID     ulid.ULID
    Epoch       int           // the fence; bumped on claim AND revocation (I-4)
    Identity    string
    ModelFamily string
    TTL         time.Duration // = k × heartbeat, k≥3 (validated at config load)
    Deadline    time.Time     // ABSOLUTE cap — Rung-3 floor, un-gameable, Flowbee-clock-only
    HBDue       time.Time
    State       LeaseState    // active | expired | revoked
}
type Manager interface {
    Claim(ctx, q store.Queries, p ClaimParams) (*Lease, error)              // 0 rows → ErrLostRace
    Heartbeat(ctx, q store.Queries, jobID ulid.ULID, epoch int, now time.Time) error // 409 on stale
    Revoke(ctx, q store.Queries, jobID ulid.ULID, reason RevokeReason) (newEpoch int, err error)
    Release(ctx, q store.Queries, jobID ulid.ULID, epoch int, d Disposition) error
}
var ErrStaleEpoch = errors.New("lease epoch stale") // → HTTP 409
var ErrLostRace   = errors.New("lost the claim race")
```

**The worker protocol (server side)** (`internal/worker`):

```go
type Registry interface {
    Register(ctx, cred auth.Credential, claimed []Capability) (Worker, error)
    Attest(ctx, workerID string, claimed []Capability) ([]Capability, time.Time, error) // returns ATTESTED subset
}
type Protocol interface {
    Lease(ctx, workerID string, roleFilter *job.Role) (*LeaseGrant, error)  // long-poll; 204 = nil
    Heartbeat(ctx, jobID ulid.ULID, in HeartbeatIn) (Directive, error)      // fenced → 409
    Result(ctx, jobID ulid.ULID, in ResultIn) (ResultOut, error)           // fenced, idempotent
    Release(ctx, jobID ulid.ULID, in ReleaseIn) error                      // fenced
}
type WorkProduct struct {
    Kind    WPKind        // patch | verdict | spec_doc
    Patch   *Patch        // diff + base_sha + blast_radius; NO pr field (Domain B, §7.3)
    Verdict *VerdictClaim // a CLAIM only — never the sign-off (I-9)
    SpecDoc *SpecDoc
}
```

`Heartbeat`/`Result`/`Release` delegate the *decision* to `engine.Decide` and apply the returned `Decision`
transactionally — the handler never makes a state decision itself.

### 3.5 The timer/lease boundary (the highest-conceptual-risk rule — state it once)

> **Reconciled:** there is **no River**. "River jobs" below are **timer-driven internal jobs** fired by the
> `timers` table + polling goroutine (§12.3). The boundary rule is unchanged in substance.

**An agent lease is NEVER a plain timer-worked/queued row.** The internal timer loop runs *internal,
trusted, bounded* units of Flowbee's own work. The agent lease is an *external, untrusted, renewable,
network-held* claim whose liveness is Flowbee's clock and whose death triggers compensation, not a silent
retry. `GET /v1/lease` runs the custom atomic claim against `jobs` (`UPDATE … WHERE state='ready' …
RETURNING`); it does **not** dequeue a timer/queue job.

The internal timer-driven jobs in Flowbee are **only**: `reconcile_sweep`, `targeted_refetch`,
`project_out_drain`, `lease_deadline_check{job_id, expected_epoch}`, `phase_deadline_check`,
`partition_grace_check`, `escalation_check`, `rung2_sweep`. A `lease_deadline_check` is **idempotent and
epoch-guarded**: it carries `expected_epoch`; on run it re-reads the job; if `lease_epoch != expected_epoch`
it **no-ops**. If still held and past deadline, it runs the revocation transaction (epoch++, compensation
enqueue, state transition) in one SQLite tx. This is the only place the timer loop and the lease meet, and it
meets correctly because the epoch guard makes a stale timer a no-op. An architecture test asserts no
registered internal job kind represents the agent's work.

---

## 4. Milestone 0 — scaffold (ordered task checklist, ready to execute)

> **As-built reconciliation:** M0 shipped on **SQLite**, not Postgres/River. Read the checklist below with
> these substitutions (per the top banner): deps = `modernc.org/sqlite` (+ stdlib `database/sql`), **not**
> pgx/river/testcontainers; no `docker-compose.yml`; `internal/store` opens a SQLite DB with
> `SetMaxOpenConns(1)` (no pgxpool, no `*river.Client`); migrations run the embedded `*.sql` only (no
> rivermigrate), `schema_migrations(version text pk, applied_at text)`; the timer loop replaces the River
> client; `/healthz` reports `{status, db, version}`. The checklist text is preserved as the original plan.

```
[ ] 0.1  go mod init github.com/swh/flowbee  (go 1.25); add deps:
         modernc.org/sqlite (pure-Go, via stdlib database/sql), oklog/ulid/v2,
         zeebo/blake3, stretchr/testify, go.uber.org/goleak, gopkg.in/yaml.v3
         (NOT: pgx/river/testcontainers — dropped; SQLite is the store)
[ ] 0.2  (no docker-compose — the store is a single embedded SQLite file; nothing to stand up)
[ ] 0.3  Makefile targets: build (CGO_ENABLED=0), migrate, serve,
         seed, lint, test, accept, fmt. DB path via FLOWBEE_DATABASE_URL (a SQLite file path).
[ ] 0.4  internal/config: Config{DatabaseURL (SQLite file), PrivateAddr=:7000, HealthAddr=:7001, WebhookAddr (unused M0),
         LeaseTTL=300s, HeartbeatInterval=30s, LongPollWait=30s, LogLevel}.
         Load() reads env, applies defaults, VALIDATES LeaseTTL >= 3*HeartbeatInterval (§6.3.3).
[ ] 0.5  internal/clock: Clock interface { Now() time.Time; NewTimer(d) Timer }; realClock + fakeClock.
[ ] 0.6  internal/ulid: monotonic minter with an injectable entropy source (testable).
[ ] 0.7  internal/store: Open() a SQLite DB (database/sql, modernc.org/sqlite, SetMaxOpenConns(1));
         Store wrapping *sql.DB + the in-process timer loop; Ping() for /healthz; Close().
[ ] 0.8  internal/store/migrate.go: //go:embed migrations/*.sql; MigrateUp() runs the embedded *.sql in
         lexical order, each in its own tx, recorded in
         schema_migrations(version text pk, applied_at text /* RFC3339 */). Idempotent.
[ ] 0.9  internal/store timers: a `timers` table (due_at, expected_epoch, fired) + ONE polling
         goroutine, started/stopped by serve (replaces the planned in-process River client).
[ ] 0.10 internal/api/server.go (health half): GET /healthz → 200 {status,db,version} / 503 on
         Ping fail. Health listener on HealthAddr; worker-API listener on
         PrivateAddr (404 stubs in M0). Two distinct *http.Server (§12.1).
[ ] 0.11 cmd/flowbee/main.go: flag dispatch on os.Args[1]:
           serve   → Load → Open → MigrateUp → start the timer loop → start health+private servers → block on
                     signal → graceful shutdown.
           migrate → Load → Open SQLite → MigrateUp → exit 0.
           work|lease|submit|seed → print usage stub (filled M1+).
           version → print build SHA.
[ ] 0.12 CI (.github/workflows or equivalent): go build, go vet, golangci-lint run,
         smoke test that boots `serve` against a temp-file SQLite DB and hits /healthz, then exits.
[ ] 0.13 tools/archcheck skeleton: walk `go list -deps` for internal/engine, internal/job, internal/ledger,
         internal/lease; fail if any depends on time(beyond type)/math/rand/crypto/rand/ULID/clock/
         github/LLM. (No core packages exist yet — wire the tool so it's green and enforced from M1.)

DONE WHEN: `flowbee serve` boots against an empty SQLite file, runs all migrations idempotently
(re-run = no-op), starts the timer loop, opens both listeners (/healthz returns 200), and tests are green in CI.
```

---

## 5. Milestone 1 — deterministic control-plane core (the lease thread)

Seed a `build` job → a stub worker registers, long-poll-leases it **exactly once** → heartbeats →
posts a result → the `job_events` ledger and `jobs` projection reflect `ready→leased→building→
review_pending` → a second concurrent worker gets **204**; any stale-epoch call gets **409**. No GitHub,
no LLM, no PR — the result transition stops at `review_pending` (PR-open is M7).

### 5.1 SQL schema (embedded migrations)

> **As-built dialect note.** The DDL below is written in the planned Postgres dialect. The shipped
> migrations are **SQLite** (`modernc.org/sqlite`). Translate as you read: `BIGINT GENERATED ALWAYS AS
> IDENTITY` / `SERIAL` → `INTEGER PRIMARY KEY AUTOINCREMENT`; `TIMESTAMPTZ … DEFAULT now()` → `TEXT` holding
> RFC3339, default `datetime('now')`; `JSONB` → `TEXT` (JSON string); `$1,$2` placeholders → `?`; the
> partial unique index and `UPDATE … RETURNING` carry over unchanged (SQLite supports both). The semantics
> — append-only ledger, per-job ordinal, the "one active lease per job" partial unique index — are identical.

**`0002_job_events.sql` — the append-only ledger (the spine):**

```sql
-- The event-sourced spine. APPEND-ONLY. The jobs row (0003) is a fold over this.
CREATE TABLE job_events (
    seq          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, -- global total order
    job_id       TEXT        NOT NULL,                            -- ULID
    job_seq      INT         NOT NULL,                            -- per-job ordinal (1,2,3,…)
    kind         TEXT        NOT NULL,                            -- ledger.EventKind
    from_state   TEXT,
    to_state     TEXT,
    lease_epoch  INT,                                             -- the fence in force when emitted
    actor        TEXT        NOT NULL,                            -- 'system' | 'reconcile' | worker identity
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,        -- kind-specific RESOLVED facts
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id, job_seq)                                      -- per-job append serialization
);
CREATE INDEX job_events_job_id_idx ON job_events (job_id, job_seq);
```

**`0003_jobs.sql` — the projection + LIVE lease columns + partial unique index:**

```sql
CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,                         -- ULID, Flowbee-minted
    kind                TEXT NOT NULL CHECK (kind IN ('spec','build')),
    flow                TEXT NOT NULL CHECK (flow IN ('spec','build')),
    stage               TEXT NOT NULL,                            -- author|review|build|merge
    state               TEXT NOT NULL,                            -- §6.2 catalogue
    role                TEXT NOT NULL,
    -- lineage (Domain A)
    chat_ref            TEXT,
    spec_ref            TEXT,
    parent_job          TEXT,
    issue_number        INT,                                      -- GitHub-owned; reconcile-IN only (M6)
    pr_number           INT,                                      -- GitHub-owned; Flowbee on PR-open (M7)
    -- SHA binding (build) — GitHub-owned (M6); seeded directly in M1 tests
    base_sha            TEXT,
    head_sha            TEXT,
    -- spec binding (spec) — M7
    spec_content_hash   TEXT,
    spec_version        INT,
    -- scheduling
    blocked_by          TEXT[] NOT NULL DEFAULT '{}',
    priority            INT    NOT NULL DEFAULT 0,
    enqueued_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- LIVE lease columns (the §6.3.1 claim mutates these in one statement)
    lease_id            TEXT,
    lease_epoch         INT  NOT NULL DEFAULT 0,                  -- monotonic fence; bumped every claim/revoke
    lease_deadline      TIMESTAMPTZ,                              -- absolute Rung-3 cap
    lease_hb_due        TIMESTAMPTZ,
    bound_identity      TEXT,
    bound_model_family  TEXT,
    bound_lens          TEXT,
    -- anti-affinity sibling pointers (populated when build spawns review/merge children; null in M1)
    eng_worker_job      TEXT,                                     -- §6.3.1 code_reviewer predicate
    code_reviewer_job   TEXT,                                     -- §6.3.1 merger predicate
    -- counters (§6.7)
    attempts            INT NOT NULL DEFAULT 0,
    max_attempts        INT NOT NULL DEFAULT 5,
    bounces             INT NOT NULL DEFAULT 0,
    max_bounces         INT NOT NULL DEFAULT 3,
    stall_revocations   INT NOT NULL DEFAULT 0,
    cost_tokens         BIGINT NOT NULL DEFAULT 0,
    cost_ceiling_tokens BIGINT,
    -- verdict (gate stages; written ONLY by gate logic, never a worker; I-9). M1: unused.
    verdict             JSONB,
    job_seq             INT NOT NULL DEFAULT 0,                   -- fold cursor: latest job_events.job_seq
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- I-4 structural backstop: at most one active lease per job, even with a logic bug (§6.3.1).
-- The active-state list MUST exactly equal the set with "active lease = yes" in §6.2.1.
CREATE UNIQUE INDEX one_active_lease_per_job
    ON jobs (id)
 WHERE state IN ('leased','building','code_review','merging',
                 'merge_handoff','spec_authoring','spec_review');

CREATE INDEX jobs_ready_idx ON jobs (state, priority DESC, enqueued_at) WHERE state = 'ready';
```

> The partial index is `ON jobs(id)` filtered to active states; since `id` is the PK it is a documentary
> tripwire in the single-row model — the **actual** exactly-once guarantee in M1 is the atomic
> `UPDATE … WHERE state='ready'` returning 0 rows to the loser (MVCC: the loser blocks, re-reads, matches
> 0 rows). The index is kept verbatim per §6.3.1 as the structural backstop and to fail loudly if the
> multi-lease model ever regresses. **Treat a `23505` unique-violation on claim as "lost the race," not a
> 500.** Run claims at the default `READ COMMITTED` — not `SERIALIZABLE` (the row lock + `WHERE state`
> already serialize correctly).

**`0004_leases.sql` — lease history/audit (append-only):**

```sql
CREATE TABLE leases (
    lease_id      TEXT PRIMARY KEY,
    job_id        TEXT NOT NULL,
    lease_epoch   INT  NOT NULL,
    identity      TEXT NOT NULL,
    model_family  TEXT,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl_s         INT  NOT NULL,
    deadline      TIMESTAMPTZ NOT NULL,
    ended_at      TIMESTAMPTZ,
    end_reason    TEXT,                                           -- completed|released|expired|revoked|superseded
    UNIQUE (job_id, lease_epoch)
);
```

**`0005_workers.sql` — registry + attested caps:**

```sql
CREATE TABLE workers (
    worker_id              TEXT PRIMARY KEY,
    identity               TEXT NOT NULL UNIQUE,                  -- bound to credential (M5)
    host                   TEXT NOT NULL,
    claimed_capabilities   TEXT[] NOT NULL,
    attested_capabilities  TEXT[] NOT NULL,                      -- M1: attested := claimed (probing is M5)
    max_concurrent_leases  INT  NOT NULL DEFAULT 1,
    attestation_expires_at TIMESTAMPTZ NOT NULL,
    registered_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**`0006_result_idempotency.sql`:**

```sql
-- A retried result returns the same response with no re-apply / no re-emit (§7.3).
CREATE TABLE result_idempotency (
    job_id          TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    response        JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, idempotency_key)
);
```

### 5.2 The state machine (M1 subset; full table in M3)

`internal/job/transition.go` — a pure, total, table-driven function. Returns `ErrIllegalTransition` for
any pair not in the table.

| cur | role | trigger | next |
|---|---|---|---|
| `ready` | eng_worker | `claimed` | `leased` |
| `leased` | eng_worker | `work_started` | `building` |
| `building` | eng_worker | `result_received` | `review_pending` |
| `leased`/`building` | * | `lease_expired_retry` (attempts<max) | `ready` |
| `leased`/`building` | * | `lease_expired_exhausted` | `needs_human` |
| `leased`/`building` | * | `released` | `ready` |

The `attempts<max` decision is resolved **outside** the pure function (the runtime picks the
`*_retry`/`*_exhausted` trigger), keeping `Next` pure. `result_received` moves state via gate logic; in M1
there is no reviewer, so the build result simply lands `review_pending` with **no PR opened** and the ack
omits `pr_number`.

### 5.3 The atomic claim SQL (§6.3.1 verbatim, parameterized)

```sql
-- one atomic statement; 0 rows = lost the race, long-poll continues
UPDATE jobs
   SET state              = 'leased',
       bound_identity     = $2,
       bound_model_family = $3,
       lease_epoch        = lease_epoch + 1,          -- monotonic fence allocation
       lease_id           = $4,
       lease_deadline     = $5,
       lease_hb_due       = $6,
       updated_at         = now()
 WHERE id    = $1
   AND state = 'ready'
   -- code_reviewer lease: exclude eng_worker's identity AND model_family (I-10). Inert in M1 (null sibling).
   AND NOT EXISTS (
        SELECT 1 FROM jobs sib
         WHERE sib.id = jobs.eng_worker_job
           AND $7 = 'code_reviewer'
           AND ( sib.bound_identity = $2 OR sib.bound_model_family = $3 ) )
   -- merger lease: exclude bound code_reviewer's identity (I-10). Inert in M1.
   AND NOT EXISTS (
        SELECT 1 FROM jobs sib
         WHERE sib.id = jobs.code_reviewer_job
           AND $7 = 'merger'
           AND sib.bound_identity = $2 )
RETURNING id, kind, role, base_sha, lease_epoch, lease_id, lease_deadline, lease_hb_due;
```

Candidate selection (which `ready` job to try) is a separate `SELECT … FOR UPDATE SKIP LOCKED` over
`jobs_ready_idx` (priority DESC, aging via `enqueued_at`); the `UPDATE`'s `state='ready'` guard is the
correctness guarantee, not the lock. The epoch-check (for fenced calls) returns 1 row iff `(job, lease_id,
epoch)` is the current live lease in an active state; 0 rows → `ErrStaleEpoch` → `409`.

### 5.4 HTTP endpoints (private worker API; auth deferred to M5, header-carried identity flagged insecure)

| Method + pattern | Handler | M1 behavior |
|---|---|---|
| `POST /v1/workers/register` | register | upsert `workers`; attested := claimed; return `{worker_id, lease_ttl_s, heartbeat_interval_s, attested_capabilities, attestation_expires_at}`. |
| `GET /v1/lease` | lease | long-poll: loop `engine.Claim` up to `LongPollWait`; on grant → 200 + §7.2 envelope; on timeout/lost-race → **204**. |
| `POST /v1/jobs/{job}/heartbeat` | heartbeat | `CheckEpoch` → 409 if stale; else `200 {"directive":"continue"}`; extend `lease_hb_due`, append `heartbeat` event. |
| `POST /v1/jobs/{job}/result` | result | `CheckEpoch` → 409; idempotency check (cached → identical response); apply `building→review_pending` via gate logic; `200 {"accepted":true,"job_state":"review_pending"}`. |
| `POST /v1/jobs/{job}/release` | release | `CheckEpoch` → 409; else → `ready`, `attempts++`, close lease row; `200 {"ok":true}`. |
| `GET /v1/events` | SSE | read-only state-transition feed (pulled forward — see §7 flag 1). |
| `GET /` | board | embedded HTML listing jobs + states. |

Long-poll loop: `engine.Claim` every ~1s up to `LongPollWait`; `ErrLostRace`/`ErrNoEligibleJob` → keep
looping until deadline, then **204**. (The planned Postgres `LISTEN/NOTIFY` wake-up optimization does not
apply on SQLite; the ~1s poll-loop is the shipped mechanism — correctness identical, see flag 5.)

### 5.5 Stub worker

`internal/worker/stub.go` — the §7.1 thin loop with `spawn(AGENT_CMD)` replaced by an echo. Registers,
long-polls, sends one heartbeat, posts an echo work-product (`{kind:patch, base_sha:<lease base>,
blast_radius:{…}}` — **no pr field**), releases. `flowbee work --stub` runs it in a loop from env
(`FLOWBEE_URL`, `FLOWBEE_MODEL_TAG`, identity). `flowbee lease` / `flowbee submit` are the real Mode-B
thin-client subcommands (one `GET /v1/lease` → print JSON; one `POST …/result` from flags/stdin) so Mode B
is wired from day one.

### 5.6 The acceptance test (`test/acceptance/lease_once_test.go`)

Real temp-file SQLite, in-process HTTP server, two concurrent stub workers:

1. seed a `ready` build job (+ `job_created` event, one tx);
2. two workers register with distinct identities + model families, long-poll the **same** job concurrently;
3. assert **exactly one** gets 200+lease, the other 204;
4. winner heartbeats (valid epoch → `continue`), posts result → `200 accepted, job_state=review_pending`;
5. projection: `state=review_pending`, `lease_id` cleared, `lease_epoch` monotonic (not reset);
6. ledger: event kinds in order; `Fold(events)` deep-equals the `jobs` projection (**determinism**);
7. a stale-`lease_epoch` heartbeat/result → **409** (zombie quarantined);
8. a fresh lease attempt now returns **204** (job is `review_pending`, not `ready`).

Supporting tests: `TestStaleEpochResult409`; `TestReleaseReturnsToReady`;
`TestNoDoubleLeaseUnderLoad` (20 goroutines / 5 jobs, each job leased to exactly one worker, run `-race
-count=50`); `TestIdempotentResultRetry` (concurrent duplicate result → one applies, both get identical
200, exactly one `review_pending` event); `TestFoldEqualsProjectionUnderConcurrency`.

**DONE WHEN:** `make accept` green; the thread `seed → lease(once) → heartbeat → result →
review_pending` is proven in the ledger AND the projection; the second worker provably gets 204; stale-epoch
calls provably get 409; `Fold(events)` provably equals the `jobs` projection; the lifecycle is visible live
via SSE.

---

## 6. Test & dev-environment strategy

### 6.1 Test taxonomy

| Lane | What | Speed | Command |
|---|---|---|---|
| **unit** | `Fold` purity, state-machine transitions, `engine.Decide`, `liveness.EvaluateKill` truth table, content checks, HMAC verify | ms, no DB | `go test ./... -short -race` |
| **replay** | golden event streams → projection; **live == replay** | ms (events are fixtures) | part of unit lane |
| **integration** | atomic claim, fencing, idempotency, reconcile/project vs `fakeGitHub`, ADOPT | seconds, real temp-file SQLite | `go test ./... -race` |
| **race** | the M1 concurrent-claim race, the no-double-lease model test, goleak | seconds, hammered | `go test ./internal/lease -race -count=50` |
| **e2e (in-proc)** | httptest server + in-process stub worker + `fakeGitHub`, full happy/sad flows | seconds | tag `e2e` |
| **e2e_github** | real sandbox repo + App creds; one smoke PR + merge | minutes, manual/nightly | tag `e2e_github`, off by default |

### 6.2 Determinism enforcement (three layers)

1. **Fold purity** — CI lint (`tools/archcheck`) forbids `time.Now`/`math/rand`/`crypto/rand`/ULID/clock/
   GitHub/LLM imports in `internal/{engine,job,ledger,lease}`. Events carry *resolved* facts (a clock-derived
   `deadline` is recorded in the `lease_claimed` event, never recomputed at fold time).
2. **Live == replay** — every scenario's live event stream (captured by driving the API) replays via
   `Fold` to the same `jobs` projection, byte-for-byte (canonical-JSON compare).
3. **The I-9 reconciled-verdict test (keystone, M3+)** — a hostile worker that lies (`status:succeeded` on a
   broken/red/SHA-moved diff) never produces a sign-off; only reconciled-true facts mint one.

### 6.3 Clock injection

`internal/clock.Clock` is injected everywhere; **never** call `time.Now()` in core. Liveness/lease-cap
tests use a fake clock with manual `Advance(d)`. For liveness, unit-test the pure decision functions
(`EvaluateKill`, the §10.3 truth table) with the fake clock; keep real-timer-loop tests few and slow.

### 6.4 Stubs

- **Stub worker** (one configurable Go type): honest / slow / hostile (lies) / zombie (POSTs after
  revocation → must 409) / partitioned (stops heartbeating). Covers most of §7 and §10.
- **`fakeGitHub`** (in-memory `github.Client`): records every call (dedupe assertions), scripts
  `BoardSweep` responses (drive supersession, CI transitions, merged-fact). No real GitHub in unit/integration.
- **git fixture**: a bare repo in a temp dir per test for epoch-ref promotion, worktree, bundle (local git
  ops, no network).

### 6.5 SQLite for tests (no Postgres, no containers)

> **As-built:** the planned testcontainers-Postgres path was **dropped**. Tests run against a **real
> temp-file SQLite DB** — hermetic, zero external services, fast.

`newTestDB(t)` creates a fresh temp-file SQLite DB (`t.TempDir()`), opens it with `SetMaxOpenConns(1)`,
runs the embedded migrations, and registers `t.Cleanup` to close it. No shared container, no truncation
footgun: each test gets its own file. The single-connection store serializes writes so the partial unique
index and the atomic claim are exercised exactly as in production. The race lane (`-race -count=50`) drives
the concurrent claim through this real store.

### 6.6 Makefile

There is **no `docker-compose.yml`** — the store is a single embedded SQLite file. Makefile:
`build migrate serve seed lint test accept fmt`. `lint` runs `golangci-lint` **plus**
`tools/archcheck` (no clock/rand/LLM/GitHub in core) **plus** `tools/providerlint` (§5.6: no provider
literal outside `model_family:*` / `lens.prompt_ref`). The green bar is
`go build ./... && go vet ./... && go test ./... -race && go run ./tools/archcheck`. A migration round-trip
test asserts re-running migrations is idempotent.

---

## 7. Implementation risks & open setup decisions

### 7.1 Resolved design inconsistencies (flag-and-decide)

The design doc carries residual inconsistencies; each is resolved pragmatically and flagged. Items **1–3**
must be settled before writing the M1 migrations; **4–8** before the milestone that exercises them.

1. **Event-sourced `job_events` ledger is mandated by the task brief but only *implied* by `DESIGN.md`.**
   The doc treats the `jobs` row as a directly-mutated record everywhere (§6.1.1, §6.3.1, §12.3) and names
   only the webhook inbox / project-OUT outbox / audit log as append-only — there is no `job_events` table
   in the spec. **Resolution:** build `job_events` as the spine, with `jobs` a fold maintained
   **synchronously in the same transaction** as each event append (not async). This preserves the §6.3.1
   atomic claim: the fence and state machine read/write the live `jobs` projection transactionally; the
   ledger is the replay/audit spine written in the same `BEGIN…COMMIT`. **An async projection would
   reintroduce a race the partial unique index can no longer prevent — do not do that.** *Confirm with Sam:
   this is a real architectural addition, not a clarification.*

2. **`leases` table vs. lease columns on `jobs`.** §6.3.1 mutates `lease_*` columns on `jobs` (and the
   partial unique index is `ON jobs`), while §12.3/§12.6 reference a `leases` table. **Resolution:** the
   **live** lease state lives on `jobs` (so the claim is one atomic statement); `leases` is an append-only
   **history/audit** row per grant/revoke (roster + audit). No contradiction once split this way; don't try
   to span the unique index across two tables.

3. **The partial unique index is a documentary tripwire in the single-row model.** Since `id` is the PK,
   `one_active_lease_per_job ON jobs(id)` cannot itself catch a double-claim of one row — the atomic UPDATE
   does. **Resolution:** keep it verbatim per §6.3.1 as the structural backstop (it becomes load-bearing if
   a future multi-lease model regresses) and document in code that M1's exactly-once guarantee rests on the
   atomic UPDATE + checking `rowsAffected==1` + mapping `23505` to "lost race."

4. **SSE timing.** §7.2/§12.2 label SSE "post-MVP," but the walking skeleton needs an observable surface.
   **Resolution:** pull a **read-only** SSE feed (`/v1/events`) forward to M1. It never touches the
   lease-delivery path (still long-poll), honoring §12.2's "never the lease path" rule. *Flagged: contradicts
   the literal "post-MVP" label.*

5. **Long-poll wakeup mechanism (not specified).** §7.2 says `GET /v1/lease` blocks ~30s. **Resolution
   (as-built):** the shipped store is SQLite, which has no `LISTEN/NOTIFY`; the lease long-poll uses a
   **~1s sleep-poll loop** up to `LongPollWait`, then `204`. (The pre-build plan's "M2 upgrades to Postgres
   `LISTEN/NOTIFY`" is moot — no Postgres.) Correctness identical; latency/efficiency only.

6. **`reconciling_verdict` appears in a §7.3 API response but not in the §6.2.1 state catalogue.** The
   `code_reviewer` result returns `job_state:"reconciling_verdict"`. **Resolution:** treat it as an
   **ephemeral status within `code_review`** (verdict reconciliation is async against the next sweep), not a
   distinct state; the canonical state stays `code_review` until the gate mints (M3/M6). Don't add it to the
   `job.State` enum.

7. **`kind` vs `flow` redundancy.** §6.1.1 carries both with identical value sets (`spec|build`).
   **Resolution:** keep both columns (`kind` is the SHA-boundary discriminant the spec→build flip mutates;
   `flow` selects the DAG); they move together. *Flagged as a future divergence point if a third flow appears.*

8. **Attestation depth.** §7.2/§9.4.1 mandate "probed/attested, not self-declared" but give no probe for
   `role:*`/`model_family:*`. **Resolution (M5):** arch/os are handshake-verifiable; `role`/`model_family`/
   `tool` start as **enrolled-identity-allowlisted** (the operator declares per-identity what a box may
   attest) with spot-checks; true capability probing (e.g. a canary build) is a named refinement. *Flagged:
   the design implies stronger probing than it specifies.*

9. **`merger`-as-agent path is lightly tested under Branch A.** §8.5.2 (batch-1) needs no custom check, yet
   §5.3/§5.4 route handoff through a distinct `merger` stage. Under Branch A every approved job takes
   `handoff`→human, so the `merger` *agent* path is effectively a human in v1; the `merger` *role* machinery
   (anti-affinity term, M4) is built but its agent-driven path is exercised only when Branch B is enabled.

10. **Spec-flow liveness with Rung-2 forced to abstain (§10.2 note).** Spec jobs have no SHA, so Rung-2
    always abstains; the two-rung rule then leaves only Rung-3. **Resolution (M8):** spec-flow soft-deadline
    crossings are corroborated only by the **absolute cap** (the lone unilateral Rung-3 kill) — i.e. spec
    jobs are effectively killable only at the hard cap. *Flagged as intended-but-confirm.*

### 7.2 Top engineering risks, ranked, with the test that retires each

1. **Async projection breaks the atomic claim** → keep the fold synchronous (flag 1). Retired by the race
   test + live==replay.
2. **Modeling the agent lease as a plain queue/timer-worked row** → a generic queue's rescuer would
   double-dispatch with no fence bump (the T2 zombie race, *caused by the queue*). Retired by §3.5's rule +
   the "no internal job kind is the agent's work" structural test + the stale-deadline-check no-op test.
   (This is also *why* the lease is a custom primitive on SQLite, not delegated to any job-queue library.)
3. **Claim predicate wrong / not checking `rowsAffected`** → double-lease. Retired by the 64-goroutine race
   test under `-race -count=50` + the unique-index population assertion.
4. **Nondeterminism leaks into the fold** → replay diverges, audit lies. Retired by `tools/archcheck` +
   golden replay + live==replay.
5. **Worker self-report becomes a verdict (I-9 violation)** → spurious merge. Retired by the hostile-worker
   reconciled-verdict test (M3+).
6. **Epoch fence doesn't reach git/CI (T2)** → zombie races a live worker to `main`. Retired by
   epoch-ref-promotion and `(job,epoch)`-CI tests against the git fixture + `fakeGitHub` (M11).
7. **Webhook trusted as authority / not replay-safe (I-1/I-2)** → unreviewed merge from a forged event.
   Retired by the forged-approval + write-ahead-replay tests (M6).
8. **Liveness false-positive kills healthy work** → retired by `EvaluateKill` unit tests over the §10.3
   truth table (every row), fake-clock driven, incl. "soft-deadline + abstain ⇒ NO kill" and "absolute cap
   ⇒ unilateral kill" (M8).

### 7.3 What Sam must provide (prerequisites & decisions)

**Available now (verified):** Go 1.25.6, git, the repo at `/Users/sam/dev/flowbee`. M0–M5 need
**nothing further** — they run entirely against an embedded SQLite file (no Docker, no Postgres) with stub
workers and a fake agent CLI. Build can start immediately.

**Needed before M6 (GitHub reconcile-IN) — the first external dependency:**

- [ ] **One outbound GitHub identity.** For the **single-operator default**, a **fine-grained, repo-scoped
      PAT** (or a `gh` token) is sufficient and recommended — reconcile-first makes the 5k/hr budget a
      non-issue (DESIGN §8.3, I-14). At **org/multi-repo scale**, use a **GitHub App** instead and provide:
      App ID, a generated private key (`.pem`), the installation ID, and the webhook secret (HMAC).
      Least-privilege permissions either way: `contents`, `pull_requests`, `issues` (read/write),
      `checks`, `statuses` (write), + the webhook HMAC secret. This is the **single outbound identity**
      (I-14) — the multi-PAT/multi-account *pool* is dropped (a single PAT is not a pool).
- [ ] **A dedicated test/sandbox repo** (throwaway, with branch protection configurable) for M6/M7 e2e —
      separate from any live repo, so the first reconcile/project-OUT runs cannot disturb real work.
- [ ] **A webhook ingress path** to the public listener (`:8443`) for the test repo — a Tailscale Funnel,
      an ngrok-style tunnel, or a reachable host. (Webhooks are only *hints*; the sweep is the floor, so this
      can be deferred to gap-driven full sweeps if ingress is hard, but it's needed to test the inbox path.)

**Needed before M7 (project-OUT writes to a live repo) — a decision:**

- [ ] **THE ONE DECISION (§14) — RESOLVED: Branch B.** The decision is made: **autonomous merge, no human
      gate**, configurable via `Policy.AllowSelfMerge` (`config.AllowSelfMerge`, env
      `FLOWBEE_ALLOW_SELF_MERGE`, `flowbee.yaml: allow_self_merge`). The code default is `false` (a fail-safe
      Branch A), but the **production posture is `true`**. The safety net is deterministic — content-integrity
      gate (I-11) + epoch side-effects (I-12) + reconciled SHA-bound verdict (I-9) + CI-green-at-head (I-5),
      all built (M9/M11). Optional: run the §13.4 measurement gate to *confirm* the posture, not to decide it.
- [ ] **Branch protection on the test repo's `main`** (required review from a distinct identity, no
      force-push) so the M7 startup assertion (I-8) and the human-merge step have something to check against.

**Needed before M12 (cross-box / production):**

- [ ] **Tailscale (or WireGuard) tailnet** spanning Flowbee's host and any worker boxes, plus the
      enrolled-identity scheme (which boxes may attest which roles).
- [ ] **SQLite WAL-mode + litestream-style continuous replication** of the DB file and a documented RPO;
      **launchd** `KeepAlive` on the macOS host. (No Postgres standby — the store is SQLite.)
- [ ] **Confirm cross-box worker hosts are trusted with the repo source** — §9.7's egress gap is *not*
      closed in v1; running an untrusted host means accepting it can leak (not escalate) repo bytes. Under
      Branch B this becomes a residual risk Sam must consciously accept.

**Open setup decisions (defaults chosen; override if desired):**

- Module path `github.com/swh/flowbee` (matches `s@swh.me`); listener ports `7000` private / `7001` health /
  `8443` webhook; lease TTL 300s / heartbeat 30s (k=10, satisfies k≥3); **embedded-SQLite store**
  (`modernc.org/sqlite`, single connection — the shipped decision; Postgres declined for the single-operator
  scale). Say the word to change any of these before M0.
