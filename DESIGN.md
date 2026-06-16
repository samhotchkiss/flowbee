# Flowbee — design (ground-up)

> **Status:** authoritative ground-up design · supersedes the "Foreman" draft (`/Users/sam/Desktop/foreman-spec.md`) · incorporates an adversarial review and several design sessions · reconciled to the built engine (M0–M12) 2026-06-16.
> **THE ONE DECISION (§14) is RESOLVED: Branch B — autonomous merge, no human gate.** `Policy.AllowSelfMerge` is the configurable toggle (`config.AllowSelfMerge`, env `FLOWBEE_ALLOW_SELF_MERGE`); the production posture is `true`. The architecture made either answer a policy flip, not a redesign — and the flip is taken. The safety net is deterministic, not a human: the content-integrity gate (I-11), CI-green-at-the-integrated-head (I-5), and the reconciled, SHA-bound verdict (I-9), with auto-revert as later hardening.
>
> **Reconciliation note (built reality vs. the draft below).** This document was drafted before the engine was built and named **River/Postgres** as the store and job-queue substrate. The shipped engine does **not** use them. The store of record is **embedded SQLite** (`modernc.org/sqlite`, pure-Go, `CGO_ENABLED=0`, one connection — `SetMaxOpenConns(1)`); River's cadence/timer/retry role is served by a **hand-rolled `timers` table + one polling goroutine**, epoch-guarded. Where the prose below still says "River" or "Postgres" as the substrate, read **"SQLite + the in-process timer/dispatch loop"**; the *semantics* the draft attributed to River (transactional enqueue coupled to the state change, idempotent epoch-guarded deadline checks, dedup, sweeper) are all preserved — only the dependency changed. §12.3 is the canonical, reconciled statement of the store.

---

## Reading guide

This is the authoritative spec, not a survey. Read §3 (two domains & invariants) as law — everything downstream is a consequence of it. The invariant IDs **I-1 … I-16** are defined once, canonically, in §15 and cited by ID elsewhere. The canonical role names, state names, credential classes, and anti-affinity terms are fixed in §5–§6 and §9 and used uniformly throughout.

**Canonical vocabulary (fixed once, used everywhere):**

| Concept | Canonical term | Notes |
|---|---|---|
| Five roles | `spec_author`, `spec_reviewer`, `eng_worker`, `code_reviewer`, `merger` | §5.2 |
| Job kinds | `spec`, `build` (exactly two) | §6.1 |
| Code-review state | `code_review` | label `flowbee:code-review`; flow stage-id `review`; board lane `code_review` |
| Build state | `building` | flow stage-id `build` |
| Outbound GitHub identity | **the single installation identity** (one GitHub App installation token) | never "pool"; §9.4, I-14 |
| Content hash (pre-SHA) | **BLAKE3** `spec_content_hash` | §11.5 |
| Store of record | **embedded SQLite** (`modernc.org/sqlite`, single conn) | §12.3; never Postgres/River |
| Determinism invariant | **flowbee-core is a deterministic, replayable function of persisted facts** | §3.6 (I-0); all intelligence is a job |
| Orchestrator | **Flowbee** | never "Foreman" |

---

## 1. One-liner

Point a GitHub repo at a fleet of your own machines running coding agents (Codex, Claude, whatever). Flowbee is the system of record for the *process* — it chats-to-spec, gates the spec, dispatches the build, gates the review, and drives the merge — handing each unit of work to whichever connected agent is *capable, idle, and independent*, across one box or many over a LAN/Tailscale, with nothing locked to any model provider.

Flowbee develops **substantial agentic software**, which is intrinsically **multi-model** (the speccer, the spec-reviewer, the builder, and the code-reviewer should not be the same model — uncorrelated failure modes are a feature you engineer for) and **multi-node** (builds, E2E suites, and long agent runs saturate hardware and want to spread across boxes). Flowbee treats both as the ground state of the problem.

---

## 2. Problem & first principles

### 2.1 The pain it removes

A clean-room redesign of a home-grown pipeline, motivated by failures actually hit:

| Today's pain | Root cause | Flowbee's answer |
|---|---|---|
| **GitHub API rate-limit outages** wedge everything | ≈5 loops + every worker poll on **one shared token**; 3 redundantly poll the same `statusCheckRollup` | **One** process talks to GitHub: a single low-frequency reconcile sweep + webhook-triggered targeted refetch, single-identity writes. Workers never call GitHub (R4). |
| **CI arch-lottery** (E2E passed on arm64, failed on the x86 iMac) | jobs dispatched to a pool with no capability matching | jobs carry **constraints derived from the diff**; workers declare **attested** capabilities; the scheduler matches only compatible, independent pairs |
| **Sprawl** (reconcile + route_prs + review_prs + codex_watch + 2 dispatchers + claim-dirs + watchdogs) | each concern grew its own loop + state | one job store, one scheduler, one worker protocol |
| **Fixed roles** (codex builds, claude reviews) | roles hard-coded in separate scripts | roles are **config**; `eng_worker=codex`, `code_review=opus` flips without touching a worker or the control plane |
| **Orphaned claims / stalls** | claim-dirs + ad-hoc liveness | first-class **fenced leases** with TTL + heartbeat + a 5-rung stall detector |
| Can't express "spec gate, then build, then a distinct-lens code gate" | single linear reviewer script | declarative **flows**: two gated flows joined at a SHA boundary |

### 2.2 The real product (what this is FOR)

The end-to-end pipeline, in the owner's (Sam's) words, is four stages — and crucially **the process begins before any code or SHA exists**:

1. Sam **chats** with an agent, who **specs** the product and **creates issues** (`spec_author`; the human entry point).
2. A **second** agent with a clear lens + identity **reviews the spec** for engineering style + project requirements; on **sign-off** it proceeds (`spec_review` **GATE** — before any code/SHA exists).
3. An **engineering worker** picks up the task and builds it (`eng_worker`).
4. A **reviewer** reviews the work: matches the original spec, matches code standards, passes CI; then **either** merges **or** hands off to a dedicated **merger** agent (`code_review` **GATE** + conditional merge handoff).

The through-line is a **lineage** no external system can hold: `chat → spec → issue → build → PR → review → merge`. Flowbee is the only place that record lives.

```
  Sam ──chat──▶ [spec_author] ──spec+issues──▶ [spec_review GATE] ──sign-off──▶
       (no SHA yet)                                    │
                                                       ▼
        [eng_worker] ──patch+baseSHA──▶ Flowbee opens PR ──▶ [code_review GATE] ──┐
              ▲                                                                    │
              └────────────── changes_requested (bounce) ◀──────────────┐         │ pass
                                                                        │         ▼
                                                          merge here  ◀─┴─▶  handoff to [merger]
```

### 2.3 Non-negotiable requirements

Every later decision is downstream of these.

- **R1 — Provider-agnostic.** Nothing is locked to any LLM provider. A provider name appears in **exactly two places**: a *capability tag* (`model:codex`, `model:opus`) the scheduler matches on, and a *lens/persona config* describing a reviewer's focus. It appears **nowhere** in Flowbee's core logic, state machine, or protocol.
- **R2 — Pull-loop workers that dial OUT.** Each worker is a thin loop: long-poll Flowbee for a lease, run whatever agent CLI it wraps, report back. Workers **dial out**; Flowbee never connects to a worker. This makes topology transparent to the protocol.
- **R3 — Flowbee is the system of record for the *process*, kept in sync with GitHub.** Flowbee owns the job DAG, stages, agent identity + lens, sign-offs/verdicts, and the §2.2 lineage. GitHub stays ground-truth only for the facts it owns. The GitHub issue/PR is a *rendering* of a Flowbee job, never its source.
- **R4 — Workers never call GitHub.** Agents sync results *back into Flowbee*; Flowbee syncs status *up to GitHub*. This kills the rate-limit storm and is a security boundary: an untrusted worker has no path to force-push `main` or exfiltrate a token it never receives.
- **R5 — Same-box OR LAN/Tailscale, transparently.** Because workers dial out (R2), the protocol does not care which. This obligates a repo-provisioning story spanning both (same-box `git worktree`; cross-box bundle or scoped read credential).
- **R6 — Bring your own models and hardware.** No managed inference, no rented fleet. The control plane assumes heterogeneous, operator-owned capacity and negotiates capability rather than provisioning it.

### 2.4 Non-goals (v1)

Not a CI system (gates *on* CI, never runs it). Not an inference provider/model host (BYO, R6). Not a general workflow engine (the SQLite store + in-process dispatch loop sit behind a clean interface; not a Temporal competitor). Not a chat UI/IDE (ingests the spec + issues a chat produced). Not multi-tenant SaaS (single operator, single repo to start). Not a replacement for GitHub branch protection (complements it, never assumes it away). **Workers are not trusted** — Flowbee's job is to *contain* a worker, not to believe it.

### 2.5 Standing design tensions (kept foregrounded)

- **T1 — May the MVP merge WITHOUT a human in the loop?** *The* decision (§14). If the human merge gate **stays**, the heavy content-integrity/side-effect machinery is premature — ship lean and first **measure `rateLimit.remaining` for a week**. If it **comes off**, content integrity + side-effect reconciliation + reconciled-verdict provenance are the **actual MVP**.
- **T2 — Exactly-once *acknowledgement* ≠ exactly-once *execution*.** Fencing (`lease_epoch`) gates calls back *into* Flowbee; git/CI/GitHub never see the token. Therefore every externally-visible action must be **idempotent against re-dispatch** (epoch-namespaced refs, `(job, epoch)`-gated CI, explicit compensation).
- **T3 — Trust the *process*, distrust the *worker* (and its data).** A returned diff is **untrusted data**: path denylist, declared blast-radius, deterministic non-LLM static checks. Reviewers judge *the diff, not the attacker's narration*. Independence is enforced structurally.
- **T4 — Liveness: the worker being alive ≠ the agent making progress.** Resolved by the 5-rung stall detector (§10) and the two-rung kill rule, distinguishing a *partitioned* worker from a *stalled* agent.

---

## 3. Two domains & core invariants (the spine)

> This is the architectural spine. Read it as law. Everything downstream — leases, flows, liveness, the side-effect machinery — is a consequence of getting this split right.

### 3.1 The two domains

Flowbee and GitHub own **disjoint kinds of fact**. The central error the draft flirted with — "GitHub is ground truth, we mirror it" — collapses once you notice that *most of what Flowbee knows cannot be represented in GitHub at all*.

**Domain A — the PROCESS (Flowbee-owned, system of record).** The shape and meaning of the work; no GitHub object holds any of it:

- the job DAG and each job's stage (`spec_author → spec_review → eng_worker → code_review → merge`) and substate;
- **agent identity + lens/persona** bound to each stage instance;
- **sign-offs and verdicts** with provenance;
- **lineage**: the causal chain `chat → spec → issue → build → PR → review → merge`, including that issue #N descends from a spec authored in a chat with no GitHub representation;
- leases, epochs, attempt/bounce counters, anti-affinity bindings, cost meters, liveness state.

**Domain B — GitHub-owned facts (GitHub is ground truth).** A sharply bounded set GitHub *physically owns* because it performs them:

- a PR exists / its number / its current head & base SHA;
- the **CI status rollup** at a given SHA;
- **merged-or-not**, and the merge commit SHA;
- branch existence and ref positions;
- the rate-limit budget (`rateLimit.remaining`).

Flowbee mirrors these, but on conflict **GitHub wins for exactly this set and nothing else.** If a fact is not on the list, GitHub does not own it — even if GitHub displays something that looks like it (a label, a review thread, a draft flag). Those are *projections Flowbee wrote*, not facts GitHub authored.

### 3.2 The inversion: GitHub-issue-as-rendering

```
   draft mental model (WRONG for the process):
        GitHub issue/PR  ──is the truth──▶  Flowbee mirror (a copy)

   Flowbee mental model:
        Flowbee job  ──projects onto──▶  GitHub issue/PR (a rendering)
              ▲
              └── reconciles IN only the GitHub-owned facts (§3.1.B)
```

Consequences:

- A human editing a label on GitHub is **not** editing Flowbee state — it is graffiti on a rendering. The project-OUT loop overwrites it (or, in ADOPT mode, deliberately not — §12.7). The label is *output*, never *input*.
- A worker self-reporting `status: succeeded` is **not** a verdict. A verdict is a Domain-A fact Flowbee *derives* from reconciled Domain-B facts plus gate logic (I-9).
- You can blow away every issue and PR and Flowbee re-renders them from the job DAG. You cannot blow away the DAG and reconstruct it from GitHub — the spec, the lens, the sign-off provenance, the chat are simply not there.

### 3.3 The two loops

Exactly two loops cross the Flowbee/GitHub boundary. **Workers never cross it** — they dial *in* to Flowbee only; Flowbee alone speaks to GitHub.

**reconcile-IN** — pull Domain-B facts, correct drift. A low-frequency batched GraphQL sweep (every 2–5 min, on boot, on gap-detection) fetches `{issues, PRs, head/base SHA, statusCheckRollup, merged, mergeCommit, rateLimit}`. Webhooks are **hints** that trigger a *targeted* refetch — never authority. Every webhook is HMAC-verified, deduped on `X-GitHub-Delivery`, written to a durable inbox before it acts. reconcile-IN writes **only** the Domain-B fact-fields; it has no authority over a stage, verdict, or lens.

**project-OUT** — render Domain-A state onto GitHub through an **outbox**: labels, status checks, comments, draft↔ready, open-PR, merge-queue enqueue. Every write is keyed `(job_id, action, head_sha)` for idempotent dedupe, uses the single installation identity, and honors `Retry-After`. project-OUT is the *only* writer of Domain-A-derived GitHub state; if reconcile-IN sees a Domain-A projection drifted, project-OUT reasserts it.

```
                 reconcile-IN  (Domain B only)
   GitHub  ─────────────────────────────────────▶  Flowbee store
   (truth   pull: PR? CI rollup? merged? SHA? budget?    (truth for
    for B)  webhooks = hints → targeted refetch           Domain A)
   GitHub  ◀─────────────────────────────────────  Flowbee store
                 project-OUT  (Domain A → rendering)
            outbox: labels / checks / comments /
            draft↔ready / open-PR / merge-queue enqueue
            keyed (job,action,head_sha), single identity
```

Routing test: **"If GitHub and Flowbee disagree about this field, who is right?"** GitHub → reconcile-IN input. Flowbee → project-OUT output. No field is both.

### 3.4 Per-field ownership (conflict-resolution table)

The operative rule. For any field, look up its **owner**; the owner wins every conflict. "Reconciled" means the non-owning side's value is treated as drift and corrected toward the owner.

| Field | Domain | Owner | On conflict |
|---|---|---|---|
| Job stage / substate (`code_review`, `building`, …) | A | **Flowbee** | GitHub cannot represent it; nothing to reconcile. |
| Spec text / spec-review verdict + lens | A | **Flowbee** | No GitHub object holds it. |
| Lineage edges (chat→spec→issue→PR→…) | A | **Flowbee** | Reconstructable only from Flowbee. |
| Agent identity / model_family / lens binding | A | **Flowbee** | Anti-affinity (§5.5, I-10) enforced here. |
| Code-review verdict / sign-off | A | **Flowbee** | Derived from reconciled B facts + gate logic (I-9). A worker self-report is *not* this field. |
| Labels (`flowbee:code-review`, …) | A→rendered | **Flowbee** | Human edit = drift; project-OUT reasserts (except ADOPT-quiesced jobs). |
| Draft ↔ ready-for-review flag | A→rendered | **Flowbee** | A projection of stage, not an input. |
| Status-check `flowbee/review-valid@SHA` | A→rendered | **Flowbee** | Flowbee emits it; GitHub re-evaluates at the integrated SHA (merge-queue fix, §8.5). |
| **PR exists / PR number** | B | **GitHub** | Flowbee *opens* the PR (project-OUT) then stamps the returned number; thereafter GitHub owns existence. |
| **Head / base SHA** | B | **GitHub** | A base/head move **supersedes** any Domain-A verdict bound to the old pair and re-arms review+CI. |
| **CI status rollup @ SHA** | B | **GitHub** | Flowbee never *computes* CI; it reads the rollup. Gate logic consumes it, never overrides it. |
| **Merged? / merge commit SHA** | B | **GitHub** | The terminal fact. A settled merge can never be re-dispatched (I-3). |
| Branch / ref existence | B | **GitHub** | *Except* the two Flowbee-owned ref classes below. |
| Flowbee build ref `refs/flowbee/<job>/epoch-<n>` | A | **Flowbee** | Epoch-namespaced; fast-forwarded onto the real branch only after epoch validation (I-7, I-12). |
| Flowbee spec ref `refs/flowbee/spec/<job>` | A | **Flowbee** | Holds the committed `spec.md`; non-epoch; the source the verdict's `spec_content_hash` binds to (§11). |
| `rateLimit.remaining` | B | **GitHub** | Drives project-OUT backoff + the cost meter. |

The asymmetry is the point: **Domain A is a long list GitHub can't even hold; Domain B is a handful of fields GitHub authoritatively owns.** Workers own *none* of these.

### 3.5 Ack ≠ execution

> **Exactly-once *acknowledgement* is not exactly-once *execution*.** (T2)

Fencing (`lease_epoch`) means at most one worker's report is *accepted* into Flowbee state per epoch. That is the limit of what fencing buys — it gates calls **back into Flowbee** and nothing else. **Git, CI, and GitHub have never heard of the token.** A revoked-but-running zombie can push a branch, trigger CI, or open a PR, and the token won't stop it.

Therefore every externally-visible action is **idempotent against re-dispatch**, enforced structurally:

- Workers do **not** push to shared refs and do **not** open PRs. They push to **epoch-namespaced** refs `refs/flowbee/<job>/epoch-<n>`; Flowbee fast-forwards the real branch **only after** validating the epoch. A stale epoch's ref is orphaned, never promoted.
- CI is gated on `(job, epoch)` statuses, so a zombie's checks can't satisfy a live job's gate.
- Revocation triggers **explicit compensation** (close the zombie's draft PR, drop its namespaced ref), not silent hope.
- The work-product channel removes any worker-supplied PR number: the result carries `{patch | bundle, base_sha}`; **Flowbee** opens the PR and stamps the number.

Anti-affinity is part of the same correctness story: a reviewer judging its own build is an execution-integrity failure, not a style preference. The canonical anti-affinity terms (§5.5, I-10) are enforced at lease time, and untrusted PR/issue prose is stripped from a reviewer's context.

### 3.6 The invariants (summary)

**I-0 — Determinism (the load-bearing architectural commitment).** *flowbee-core (the control plane's decision logic AND the worker harness) is a deterministic, replayable function of persisted facts; all intelligence is a job.* The core (`internal/{engine,job,ledger,lease,scheduler,flow}`) takes an immutable folded `EngineState` plus one triggering `Event` and returns a `Decision` — a *description* of transitions, side-effects, and timers — with zero I/O, no clock read, no ID minting, no randomness, and no LLM/GitHub access; time and IDs are injected as values. The same event log replays to the same `Decision` stream, byte-for-byte. This is enforced **mechanically** by `tools/archcheck`, which forbids the core packages from importing `time` (beyond the type), `math/rand`, `crypto/rand`, any ULID minter, any clock, or any LLM/agent/GitHub package (the deterministic-hash exception for `crypto/sha256`/BLAKE3 aside). An LLM's `status: succeeded` enters the core only as a persisted *claim* event; the gate derives the real verdict from reconciled facts (I-9). I-0 is the invariant that makes replay, audit, and the two-domain reconciliation tractable; its removal turns the orchestrator into an unauditable, unreplayable black box.

The full canonical statement of invariants **I-1 … I-16** lives in §15 (with **I-0** added there as the determinism invariant). They hold even at MVP; their removal reintroduces the exact wedge/double-merge/unreviewed-merge failures Flowbee exists to delete. THE ONE DECISION (§14) **is resolved — Branch B, autonomous merge** — so I-9/I-11/I-12 are load-bearing on day one (the configurable `Policy.AllowSelfMerge` defaults `false` in code for safety but is set `true` in the production posture); resolving it never weakened any invariant.

---

## 4. Architecture

Flowbee is one long-lived Go process: the brain and the only GitHub caller. Workers are thin, untrusted, outbound clients. Two listeners, sharing no trust path (§12.1): a **public, HMAC-only webhook endpoint** and a **mutual-auth worker API** bound to loopback/Tailscale.

```
   GitHub ──webhook(HMAC, X-GitHub-Delivery dedupe)──▶ inbox (write-ahead)   [PUBLIC listener]
   GitHub ──reconcile-IN sweep (every 2–5 min)───────▶ ┌──────────────────────────────────────┐
        (Domain-B facts: PR/SHA/CI/merged/budget)      │              FLOWBEE                   │
                                                        │  board mirror ◀ reconcile-IN + hints  │
   GitHub ◀──project-OUT (outbox, single installation  │  (SQLite store, Domain A SoR)          │
        identity, (job,action,head_sha) dedupe,        │  scheduler (job DAG + capability +    │
        Retry-After backoff)──────────────────────────▶│   anti-affinity + aging)              │
                                                        │  custom fenced-lease primitive        │
                                                        │  5-rung liveness ladder               │
                                                        └────────▲──────────────────────▲───────┘
                                                  HTTP/JSON long-poll, mutual-auth   [PRIVATE listener]
                ┌──────────────────────┬──────────────────────┬───────────────────────┘
          ┌─────┴──────┐         ┌─────┴──────┐         ┌──────┴─────┐         ┌──────────────┐
          │ worker     │         │ worker     │         │ worker     │         │ worker       │
          │ codex      │         │ codex      │         │ opus       │         │ (merger)     │
          │ eng_worker │         │ eng_worker │         │ code_review│         │              │
          │ x86 mini   │         │ arm Studio │         │ Tailscale  │         │ LAN          │
          └────────────┘         └────────────┘         └────────────┘         └──────────────┘
            worktree off            worktree off           bundle OR              bundle OR
            shared mirror           shared mirror          scoped-read cred       scoped-read cred
```

Three structural facts to read off the diagram:

1. **One GitHub caller.** All inbound facts arrive through reconcile-IN (with webhooks as hints); all outbound writes leave through project-OUT under the single installation identity. There is no second GitHub actor, which is why the §3.4 dedupe key and the §8.4 outbound concurrency cap are trivially enforceable and the rate-limit storm cannot recur.
2. **Workers are outbound-only.** Flowbee holds no address for any worker — only a credential allowlist and a lease table. Same-box (loopback) and cross-box (LAN/Tailscale) differ only in *repo provisioning* and *network substrate*, never in the wire protocol.
3. **SQLite underneath, the custom lease on top.** The embedded **SQLite** store (`modernc.org/sqlite`, single connection) plus an in-process timer/dispatch loop supplies transactional enqueue, retries, timers, and a sweeper; the renewable-TTL + heartbeat + fencing **agent lease is a custom primitive on top** — agent leases are *not* plain queued rows (§12.3). *(The pre-build draft named River/Postgres here; the shipped engine uses SQLite + a hand-rolled `timers` table. See §12.3.)*

---

## 5. Flows, roles, identities & lenses

The pipeline is **two gated flows joined at a SHA boundary** — a spec flow that runs *before any code exists*, and a build flow that runs *after*. Both are the same Flowbee primitive: a declarative DAG of **stages**, each stage bound to a **role**, each role resolved to an `(identity, lens, model_family)` triple at lease time.

### 5.1 Two flows, not one

The spec flow's verdicts are **pure Domain-A state** (GitHub has no object for "a spec was signed off by a distinct reviewer lens"); the build flow's verdicts are reconciled against Domain-B facts. The only thing that makes the spec flow special: its stages carry no `base_sha` and emit no *epoch-namespaced* git ref (they do emit the Flowbee-owned spec ref `refs/flowbee/spec/<job>`, §11); its work-product channel carries prose and issue bodies, not patches.

```
        ┌──────────────── SPEC FLOW (kind=spec; no base_sha) ───────────────┐
 Sam ─chat─▶│ spec_author ──▶ [spec_review GATE] ──sign-off──▶ materialize  │
        │     (stage:author)   (stage:review)               issue(s)         │
        └────────────────────────────────┬─────────────────────────────────┘
                                          │  on sign-off: kind flips spec→build; first base_sha assigned
        ┌──────────────────────────────────▼───────── BUILD FLOW (kind=build; SHA-bearing) ─────┐
        │ eng_worker ──▶ [code_review GATE] ──┬── self_merge ───────────────────▶ merged         │
        │ (stage:build)   (stage:review)      │   (Flowbee merges, attributed to reviewer verdict)│
        │                                     └── handoff ──▶ merger ──merge──▶ merged            │
        │                                          (stage:merge; §5.4 branch point)               │
        └──────────────────────────────────────────────────────────────────────────────────────┘
```

**State / stage / label mapping** (canonical — every section maps to this):

| Flow | Stage-id (YAML) | Job state (§6) | GitHub label rendering | Board lane (§12.6) |
|---|---|---|---|---|
| spec | `author` | `spec_authoring` | `flowbee:spec-authoring` | `spec_authoring` |
| spec | `review` | `spec_review` | `flowbee:spec-review` | `spec_review` |
| build | `build` | `building` | `flowbee:building` | `building` |
| build | `review` | `code_review` | `flowbee:code-review` | `code_review` |
| build | `merge` | `merging` / `merge_handoff` | `flowbee:merging` | `merging` |

### 5.2 The five roles

A **role** is a named slot in a flow: a set of requirements the scheduler matches a worker against, plus a lens the worker is instructed to adopt. Workers are swappable into roles; the role definition carries Flowbee's guarantees.

| # | Role | Flow | Consumes | Produces (work-product) | GATE? | Verdict reconciled against |
|---|------|------|----------|------------------------|:---:|----|
| 1 | `spec_author` | spec | chat transcript | spec doc + draft issue bodies (`spec_doc`) | no | — (entry point) |
| 2 | `spec_reviewer` | spec | spec doc (clean) + diff | `signed_off` \| `changes_requested` (`verdict`) | **yes** | pure Flowbee record, bound to `spec_content_hash` (no GitHub fact exists) |
| 3 | `eng_worker` | build | signed-off issue + base SHA | patch/bundle + declared blast-radius (`patch`) | no | — |
| 4 | `code_reviewer` | build | diff (stripped) + spec lineage | `approved`+disposition \| `changes_requested` (`verdict`) | **yes** | reconciled `(head_sha, base_sha)` + CI rollup |
| 5 | `merger` | build | approved job + integrated SHA | merge request (Flowbee enqueues) | no (executor) | reconciled `merged` fact from GitHub |

Three load-bearing, **enforced** properties:

- **Each role resolves to a distinct `identity`** (a Flowbee-enrolled, credential-bound principal). Two roles in one job instance may never resolve to the same identity where anti-affinity applies (§5.5).
- **Each role declares a `lens`** — a persona/focus config. The lens is the *only* place a provider's flavor may legitimately appear, and even there only as instruction text, never as control flow.
- **Each role requires a `model_family` capability tag**, not a model. `model:codex` / `model:opus` are tags a worker advertises and Flowbee *probes/attests* (§5.6); they parametrize matching, never branch Flowbee's logic.

### 5.3 The extended flow/role YAML

Roles are defined once (provider-agnostic); flows reference them; `when:` predicates gate conditional stages; anti-affinity and the tamper-evident sign-off rule are first-class fields the engine enforces.

```yaml
# ── ROLES: pure capability + lens slots. No provider appears in CONTROL position. ──
roles:

  spec_author:
    requires:    [ "role:spec_author", "model_family:*" ]
    lens:        { persona: product_speccer, focus: completeness+testability,
                   prompt_ref: lenses/spec_author.md }
    emits:       spec_doc                                    # work-product kind: prose

  spec_reviewer:
    requires:    [ "role:spec_reviewer", "model_family:*" ]
    lens:        { persona: staff_engineer, focus: eng_style+project_requirements,
                   prompt_ref: lenses/spec_review.md }
    gate:        true
    context:     spec_and_diff_only                          # NOT the raw chat as authority (§11.2)
    emits:       verdict                                     # signed_off | changes_requested

  eng_worker:
    requires:    [ "role:eng_worker", "model_family:*" ]
    lens:        { persona: implementer, focus: make_it_pass_the_spec,
                   prompt_ref: lenses/eng_worker.md }
    emits:       patch                                       # work-product kind: diff + base_sha

  code_reviewer:
    requires:    [ "role:code_reviewer", "model_family:*" ]
    lens:        { persona: critical_reviewer, focus: spec_match+code_standards+ci,
                   prompt_ref: lenses/code_review.md }
    gate:        true
    grants:      [ merge_request_authority ]                 # MAY request self_merge (see §5.4)
    context:     diff_only                                   # strip untrusted PR/issue prose (I-10)
    emits:       verdict

  merger:
    requires:    [ "role:merger", "model_family:*" ]
    lens:        { persona: integrator, focus: clean_merge_only,
                   prompt_ref: lenses/merger.md }
    emits:       merge_request                               # executor; Flowbee enqueues to merge queue

# ── FLOWS: DAGs of stages. Each stage binds a role. ──
flows:

  spec:                                                      # job.kind = spec
    entry: chat                                              # Sam chats; no base_sha
    stages:
      author:
        role: spec_author
      review:
        role: spec_reviewer
        needs: [ author ]
        decision: sign_off                                   # distinct-lens sign-off, bound to spec_content_hash
        on_signed_off: materialize_issues                    # Flowbee opens GitHub issue(s); kind flips spec→build
        on_changes_requested: { bounce_to: author, max_bounces: 3 }
    independence:
      - "spec_author.lens != spec_reviewer.lens"             # intra-spec-flow, schedulable (I-10)

  build:                                                     # job.kind = build
    entry: signed_off_issue                                  # gains base_sha here
    stages:
      build:
        role: eng_worker
        work_product:
          channel: patch                                     # result carries diff + base_sha; NO pr field
          push_to: "refs/flowbee/{job}/epoch-{lease_epoch}"  # epoch-namespaced ref
          declare: blast_radius                              # required: paths + scope
      review:
        role: code_reviewer
        needs: [ build ]
        context: diff_only                                   # strip narration (§9.3)
        require:                                             # ALL reconciled-true before sign-off
          - ci_green_at_head                                 # from GitHub rollup, reconciled
          - static_checks_pass                               # deterministic, non-LLM (§9.2)
          - path_denylist_clear                              # else → forced handoff/human gate
        decision: distinct_signoff                           # tamper-evident, §5.5 / I-9
        on_approved:
          branch: review_disposition                         # ◀── THE BRANCH POINT (§5.4)
        on_changes_requested: { bounce_to: build, max_bounces: 3 }
      merge:
        role: merger
        when: "review.disposition == 'handoff'"              # self_merge arm skips this stage
        require: [ review_valid_at_integrated_head ]         # merge queue re-runs CI, NOT review (§8.5)
    independence:                                            # canonical anti-affinity set (I-10)
      - "eng_worker.identity     != code_reviewer.identity"  # no self-review
      - "eng_worker.model_family != code_reviewer.model_family"  # no same-model collusion
      - "code_reviewer.identity  != merger.identity"         # approver != integrator
    signoff:
      kind: tamper_evident
      source: reconciled_github_state                        # NEVER a worker status:succeeded (I-9)
      binds: [ head_sha, base_sha ]                          # verdict invalidated on any SHA move
```

Note what is **absent**: nowhere does a flow say `if model == codex`. Pinning a role to a provider in config is exactly the lock-in this design forbids. Providers enter only via `model_family:*` matching and the `prompt_ref` lens text.

### 5.4 The branch point: `code_reviewer` self-merge OR handoff to `merger`

After `code_reviewer` reaches `approved`, the flow takes one of two arms — a genuine branch in the DAG, decided by the reviewer's disposition *under policy*.

> **Crucial R4 clarification — who physically merges.** Workers never call GitHub (R4). Neither arm has a worker touch GitHub. In **both** arms the actual merge is **Flowbee enqueuing the PR to GitHub's merge queue via project-OUT**, and the merge commit is reconciled back (§8.5). The two arms differ only in *provenance and policy*:
> - **`self_merge`**: Flowbee enqueues the merge **attributed to the `code_reviewer`'s verdict**, with **no distinct `merger` stage**. ("Reviewer merges" means *Flowbee merges under the reviewer's verdict provenance*, not that the reviewer worker calls GitHub.)
> - **`handoff`**: a distinct `merger` stage (its own identity + lens) owns the merge request as a separate, reconcilable action with provenance separate from the approval. Flowbee still performs the enqueue.

```
 code_review GATE ─ approved ─┬─ disposition == "self_merge" ──▶ Flowbee enqueues merge   ─▶ merged
                              │     (attributed to reviewer verdict; no merger stage;        (Flowbee
                              │      requires the §5.4 eligibility predicate)                 reconciles
                              │                                                               merged fact)
                              └─ disposition == "handoff"     ──▶ merger stage ─▶ Flowbee   ─▶ merged
                                    (policy may FORCE this arm)      enqueues merge
```

**The single canonical `self_merge` eligibility predicate** (every other section cites this — does not restate a partial version):

> `self_merge` is permitted **iff ALL** of:
> 1. **THE ONE DECISION has removed the human merge gate** (§14). If the gate stays, `self_merge` is policy-disabled and *every* approved job takes `handoff`→human.
> 2. `path_denylist_clear` — the diff touches no denylisted path (§9.2).
> 3. `blast_radius_consistent` — declared blast-radius matches the actual diff (§9.2).
> 4. `static_checks_pass` — deterministic non-LLM checks are green (§9.2).
> 5. `integrated_head == reviewed_head` — no base/head move since review (else supersede + re-arm, §8.5).
> 6. the `code_reviewer.identity != merger.identity` term remains satisfiable for any fallback.

If **any** condition fails, the flow takes `handoff` regardless of the reviewer's requested disposition. Conditions 2–5 are exactly the content-integrity gate (I-11) and the SHA-pair binding (I-5); condition 1 is the policy toggle of T1.

| Forcing condition | Why self_merge is denied |
|---|---|
| Human merge gate is ON (§14, Branch A) | every approved job goes to a human via `handoff`. |
| `path_denylist` would be touched (CI config, `.github/workflows`, lockfiles+postinstall, Dockerfiles, secrets, Flowbee's own code) | requires a human gate or a dedicated integrator; an LLM reviewer may not wave it through. |
| Integrated head ≠ reviewed head (stack parent merged, base moved) | the queue re-runs CI but **not** review; `review-valid` must re-validate at the integrated SHA (§8.5). |
| `code_reviewer.identity == merger.identity` would result | anti-affinity: approver must not also be the integrator of a denylist-adjacent merge. |
| merge-queue batch-size > 1 (v1.1) | until the `review-valid` check re-runs at the integrated head, route through `merger` so integration is serialized and observable. |

### 5.5 Enforced anti-affinity & the tamper-evident sign-off rule

Independence keeps a worker from approving its own work and keeps two instances of the same model from collusively rubber-stamping. Flowbee enforces **four canonical terms** as hard scheduler constraints, evaluated *at lease time* — a lease that would violate any term is simply not granted (the job stays `ready`; a `no_eligible_worker` timer arms if no compliant worker exists).

```
CANONICAL ANTI-AFFINITY TERMS (capability-algebra negations on the active job instance) — I-10
──────────────────────────────────────────────────────────────────────────────────────────────
  eng_worker.identity     != code_reviewer.identity        # no self-review
  eng_worker.model_family != code_reviewer.model_family    # uncorrelated failure modes
  spec_author.lens        != spec_reviewer.lens            # independent spec lens (intra-spec-flow)
  code_reviewer.identity  != merger.identity               # approver != integrator
```

(The spec-flow lens term compares `spec_author` vs `spec_reviewer` — both actors live in the *same* flow instance, so the scheduler can evaluate it. A cross-flow `spec_reviewer.lens != code_reviewer.lens` term would be unenforceable within one flow instance and is **not** used.)

Implementation: when the scheduler offers a `code_reviewer` lease for job `J`, it reads the already-bound `eng_worker` identity+family from `J`'s lineage and adds `NOT identity:X AND NOT model_family:F` to the match predicate; when it offers a `merger` lease, it additionally reads the bound `code_reviewer` identity and adds `NOT identity:Y`. The constraints are on the **job instance**, not global — the same worker may review *other* jobs it didn't build. (The atomic-claim SQL that enforces this is §6.3.1.)

**The tamper-evident sign-off rule** (per I-9, stated canonically here; other sections cite it):

> A sign-off is a Flowbee-authored record derived from **reconciled GitHub state** and bound to a `(head_sha, base_sha)` pair (build flow) or a `spec_content_hash` (spec flow, §11). It is **never** a worker-self-reported `status: succeeded`.

A worker's `result` POST carries the reviewer's verdict and reasoning, but Flowbee treats it as a *claim*, fenced by `lease_epoch`. Flowbee then independently: reconciles the Domain-B facts the gate requires (`ci_green_at_head`, PR exists at the expected SHA pair, no drift); runs the deterministic static checks + path-denylist scan against the diff (untrusted data, §9.2); and **only then** mints the tamper-evident, SHA-bound sign-off and projects it OUT as a Flowbee-controlled status check. Any base/head SHA move supersedes the sign-off and re-arms the gate. The strongest move a compromised worker has is to *withhold or fabricate a verdict claim* — which fails reconciliation and never becomes a sign-off.

### 5.6 How provider-neutrality is *enforced*

Swappability is the threat to neutrality. Neutrality is therefore a set of enforced invariants, not an aspiration:

1. **No provider name in control position.** A CI lint over the flow/role configs and the engine source rejects any provider literal (`codex`, `opus`, `claude`, …) outside two allowlisted positions: a `model_family:*` capability tag and a `lens.prompt_ref` file. Found in any predicate or branch → build fails.
2. **Matching is on `model_family`; capabilities are probed/attested.** A role `requires` a *family* (often `*`); the scheduler matches workers by **attested** capabilities (§7.2), never by self-declared strings. "Probed" and "attested" name the same server-verified set (canonical term: **attested**).
3. **Work-product is provider-shape-agnostic.** The result channel carries a *diff + base SHA* (build) or a *prose verdict* (review/spec) — never a provider-specific transcript blob downstream logic parses. Flowbee opens the PR and stamps the number; the worker never supplies a PR field.
4. **The lens is the only provider-flavored surface, and it is data.** A lens may say "use Codex's apply-patch format" — instruction text loaded from `prompt_ref`, consumed by the agent CLI, invisible to Flowbee's scheduler and state machine.
5. **Independence terms are model-family-aware by design.** Because `eng_worker.model_family != code_reviewer.model_family` is enforced, the *system* structurally uses ≥2 providers on any reviewed job. A single-provider fleet cannot satisfy the build flow's independence terms and surfaces a `no_eligible_worker` alarm rather than silently collapsing review independence.

Neutrality test: take a passing build run with `eng_worker=codex, code_reviewer=opus`, swap the capability tags, change nothing else. If it still type-checks, satisfies all four anti-affinity terms, and produces a SHA-bound reconciled sign-off, neutrality holds.

---

## 6. Job model, leases & state machine

The **job** is the atomic unit of Flowbee's process state-of-record (Domain A). Everything Flowbee *knows* hangs off a job row; everything Flowbee *does to GitHub* is a projection of a job's state.

### 6.1 What a job is

A job is **one stage-instance of one flow**, carrying its own state, lease, lineage edges, and counters. It is *not* a GitHub issue, *not* a PR, *not* a worker, *not* a model. Two job **kinds** exist (exactly two — `spec` and `build`), distinguished by whether a SHA can exist yet:

| | `spec` job | `build` job |
|---|---|---|
| Flow | spec (§5.1) | build (§5.1) |
| Has `base_sha`? | **no** — runs before any code exists | **yes** — gains `base_sha` at entry |
| Flowbee-owned git ref it emits | `refs/flowbee/spec/<job>` (the committed `spec.md`, non-epoch) | `refs/flowbee/<job>/epoch-<n>` (epoch-namespaced) |
| Work-product channel | prose (spec doc, issue bodies) | patch/bundle + `base_sha` |
| Verdict reconciled against | **pure Flowbee record** bound to `spec_content_hash` — no GitHub fact exists (I-9) | reconciled `(head_sha, base_sha)` + CI rollup (I-5, I-9) |
| GATE stage | `spec_review` | `code_review` |

(The stage and role — not the kind — distinguish `spec_author` from `spec_reviewer`. `job.kind` is only ever `spec` or `build`.) The kind flips `spec → build` exactly once, when a spec sign-off **materializes issues** (§5.3 `on_signed_off: materialize_issues`). That flip is the SHA boundary: before it, verdicts are Domain-A-only; after it, verdicts are SHA-bound and reconciled.

#### 6.1.1 The job record

```
job {
  id              : ULID                 # Flowbee-minted, immutable
  kind            : spec | build         # EXACTLY two values
  flow            : spec | build         # which DAG (§5.3)
  stage           : enum                 # current stage-id within the flow (author|review|build|merge)
  state           : enum                 # the state-machine state (§6.2)
  role            : spec_author | spec_reviewer | eng_worker
                  | code_reviewer | merger     # role bound to THIS stage

  # ── lineage (Domain A — reconstructable from nowhere else) ──
  lineage {
    chat_ref      : opaque               # the chat that started the spec (no GitHub object)
    spec_ref      : ULID?                # the spec doc this descends from
    issue_number  : int?                 # GitHub issue # AFTER materialization (Flowbee-stamped)
    pr_number     : int?                 # GitHub PR #   AFTER Flowbee opens it (Domain B owns it)
    parent_job    : ULID?                # DAG edge: e.g. build job's review child
  }

  # ── SHA binding (build kind only) ──
  base_sha        : sha?                 # GitHub-owned; the base the patch applies to
  head_sha        : sha?                 # GitHub-owned; current PR head; a move SUPERSEDES verdicts

  # ── spec binding (spec kind only) ──
  spec_content_hash : blake3?            # BLAKE3 of spec.md @ refs/flowbee/spec/<job> HEAD (§11.5)
  spec_version      : int?               # ordinal on the spec branch

  # ── dependencies / scheduling ──
  blocked_by      : [ULID]               # DAG predecessors; job is `ready` only when all are `done`
  priority        : int
  enqueued_at     : ts                   # for aging (§6.6)

  # ── lease (custom primitive, §6.3) ──
  lease           : Lease?               # null unless an active state

  # ── escalation counters (§6.7) ──
  attempts        : int   ;  max_attempts  : int     # lease-level retries (failure/expiry)
  bounces         : int   ;  max_bounces   : int     # gate-level reject loops
  stall_revocations : int                            # Rung-4 governor counter (§10), distinct from above
  cost_tokens     : int   ;  cost_ceiling  : {tokens, usd}   # per-job; flow rollup too (I-15)

  # ── anti-affinity bindings (Domain A — enforced at lease time, I-10) ──
  bound_identity      : principal?
  bound_model_family  : tag?
  bound_lens          : tag?

  # ── verdict (gate stages only; written ONLY by gate logic, never by a worker, I-9) ──
  verdict         : { value: signed_off|changes_requested|approved,
                      disposition: self_merge|handoff,          # code_review only (§5.4)
                      binds: { head_sha?, base_sha?, spec_content_hash? },  # build OR spec binding
                      provenance: reconciled, tamper_evident: true }?
}
```

Field ownership follows §3.4 strictly: `issue_number`, `pr_number`, `base_sha`, `head_sha`, and the CI rollup the verdict consumes are **GitHub-owned** and only ever written by reconcile-IN; everything else is **Flowbee-owned**. A worker's `result` POST can move `state` (via gate logic) but can never write a verdict, a SHA, a PR number, or a `spec_content_hash` directly. The `verdict.binds` object holds `{head_sha, base_sha}` for build-flow verdicts and `{spec_content_hash}` for spec-flow verdicts — the unified binding field that lets §11's pre-SHA supersession live in the canonical record.

### 6.2 The full state machine

States are partitioned into spec-flow, build-flow, shared cross-cutting (reachable from either flow), and terminal. `needs_human`, `blocked`, and `superseded` are explicitly **non-terminal**.

#### 6.2.1 State catalogue

| State | Flow | Active lease? | Meaning | Exits to |
|---|---|:---:|---|---|
| `spec_authoring` | spec | yes | `spec_author` drafting spec + issue bodies | `spec_review`, (lease fail →) `ready`, `cancelled` |
| `spec_review` | spec | yes | `spec_reviewer` gate: distinct lens judges spec | `ready`(build) on sign-off, `spec_authoring` on reject, `needs_human` |
| — *(materialize)* | spec→build | — | sign-off → Flowbee opens issue(s); `kind` flips; `base_sha` assigned | `ready` |
| `ready` | build | no | all `blocked_by` are `done`; awaiting a compliant worker (§6.6) | `leased` |
| `leased` | build | yes | atomically claimed; epoch allocated; worker not yet started | `building`, (expiry →) `ready` |
| `building` | build | yes | `eng_worker` producing patch + blast-radius | `review_pending`, reject/expiry →`ready`, `superseded`, `cancelled` |
| `review_pending` | build | no | work-product received; Flowbee opened the PR, stamped `pr_number`; awaiting reviewer | `code_review` |
| `code_review` | build | yes | `code_reviewer` gate: spec-match + standards + CI, on `diff_only` context | `mergeable`, `building`(bounce), `needs_human`, `superseded` |
| `mergeable` | build | no | gate passed; reconciled CI-green + review-valid bound to `(head,base)` | `merging`, `merge_handoff`, `superseded` |
| `merging` | build | yes | self_merge arm: Flowbee enqueues the merge, attributed to the reviewer verdict (§5.4) | `done`, (fail→bounded retry→) `needs_human`, `superseded` |
| `merge_handoff` | build | yes | handoff arm: dedicated `merger` requests the merge; Flowbee enqueues (§5.4) | `done`, (fail→) `needs_human`, `superseded` |
| `done` | both | no | **terminal.** merge commit reconciled from GitHub (I-3, I-5) | — |
| `blocked` | both | no | a `blocked_by` predecessor not yet `done` | `ready` (when cleared) |
| `needs_human` | both | no | **NON-terminal.** counters/cost exhausted, or two-rung kill (I-13) | `ready` (resumed), `cancelled` |
| `superseded` | build | no | **NON-terminal.** a new head/base SHA invalidated this job's SHA-bound verdict (I-5) | `ready` (re-armed), `cancelled` |
| `cancelled` | both | no | **terminal.** explicitly cancelled or spec abandoned | — |

#### 6.2.2 State diagram

```
  ┌──────────────────── SPEC FLOW (kind=spec; no SHA) ────────────────────┐
  │  Sam·chat                                                              │
  │      ▼                                                                 │
  │  spec_authoring ──draft──▶ spec_review ──sign_off──▶ (materialize:     │
  │      ▲                       │  │           open issue, stamp #,        │
  │      └── changes_requested ──┘  │           kind:spec→build, base_sha)  │
  │            [max_bounces]        └──▶ needs_human                        │
  └────────────────────────────────────────────┬──────────────────────────┘
                                                │  (now SHA-bearing)
  ┌──────────────────── BUILD FLOW (kind=build) ▼──────────────────────────┐
  │   blocked ──deps cleared──▶ ready ──atomic claim+epoch──▶ leased        │
  │      ▲                       ▲  ▲                            │          │
  │      │(blocked_by not done)  │  │ lease_expired (Rung-3 cap) ▼          │
  │      │                       │  │ [unless attempts/cost      building   │
  │      │   changes_requested   │  │  exhausted → needs_human]      │       │
  │      │   (bounce)[max_bounces]│  └────────────────────────────────┤      │
  │      │                       │              work-product received ▼      │
  │      │                       │            (Flowbee opens PR) review_pending
  │      │                       │                                   │       │
  │      │                       │                                   ▼       │
  │      │                       │   gate pass (reconciled       code_review │
  │      │                       │   CI-green + review-valid @    │  │       │
  │      │                       │   (head,base))                 │  └─▶ needs_human
  │      │                       │                                ▼          │
  │      │                       │                            mergeable      │
  │      │                       │       disposition:  ┌──self_merge──┐      │
  │      │                       │                     ▼              ▼      │
  │      │                       │                  merging      merge_handoff│
  │      │                       │      (Flowbee      │  merge fail  │ (merger;
  │      │                       │       enqueues,    │  →needs_human │  review-valid
  │      │                       │       attributed   │              │  re-checked
  │      │                       │       to reviewer) ▼              ▼  @ integ.SHA)
  │      │                       │              ┌─ reconcile merged fact ─┐  │
  │      │                       │              │      done (TERMINAL)    │  │
  │      │                       │              └─────────────────────────┘  │
  │  ─ ─ ┴ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┴ ─ CROSS-CUTTING (from any build state) ─ ─│
  │   superseded ◀─ new head/base SHA (I-5) ─[from leased|building|         │
  │      ├─ re-arm ─▶ ready                    code_review|mergeable|        │
  │      └─ cancelled (TERMINAL)               merging|merge_handoff]        │
  │   needs_human ◀─ max_attempts|max_bounces|cost_ceiling|two-rung kill     │
  │      ├─ resume ─▶ ready    └─ cancelled (TERMINAL)                       │
  │   cancelled (TERMINAL) ◀─ explicit cancel / spec abandoned [any non-term]│
  └─────────────────────────────────────────────────────────────────────────┘
```

#### 6.2.3 The reject loops (first-class, bounded)

Two reject loops, both bounded by counters (I-6):

- **Spec-review reject** (`spec_review → spec_authoring`): the `spec_reviewer` returns `changes_requested`; the job bounces to `spec_author` with the verdict's reasoning. Bounded by `max_bounces`; on exhaustion → `needs_human`. No SHA, no GitHub side-effect — pure Domain-A churn.
- **Code-review bounce** (`code_review → building`): the `code_reviewer` returns `changes_requested`; the job bounces to `eng_worker` to re-build against the *same* issue and base. Bounded by `max_bounces`. On bounce, Flowbee transitions the rendered PR back to **draft** (project-OUT) and drops the stale epoch ref; the next attempt pushes to a **new** epoch namespace.

A bounce (verdict-driven) is distinct from a lease failure (attempt-driven). They increment **different** counters (`bounces` vs `attempts`) with **different** ceilings.

#### 6.2.4 `superseded` — the SHA-move state

`superseded` realizes I-5 and the §3.4 rule that a base/head move supersedes any Domain-A verdict bound to the old pair. When reconcile-IN observes a new `head_sha`/`base_sha` for a job's PR, Flowbee: (1) transitions the job (from any active or `mergeable` state) to `superseded`; (2) invalidates the SHA-bound `verdict`; (3) revokes any active lease (epoch bump → §6.3) and runs compensation (§6.5); (4) re-arms by routing to `ready` with the new `base_sha`, re-running review + CI from scratch. `superseded` is never reached by worker action — only by reconcile-IN seeing a GitHub-owned SHA change. (The spec-flow analogue — a `spec_content_hash` change superseding a spec sign-off — is in §11.5.)

### 6.3 Exactly-once fenced leasing (the custom primitive)

The SQLite store + in-process timer loop supply transactional enqueue, retries, timers, and a sweeper — but a queued/timer row is *not* the agent lease. The renewable-TTL + heartbeat + fencing agent lease is a **custom primitive on top of the store** (§12.3). A long-poll `GET /v1/lease` does not dequeue a generic job row; it competes for a `ready` job row (`UPDATE … WHERE state='ready' … RETURNING`) and, on win, mints a `Lease` with an epoch.

#### 6.3.1 Atomic claim (with full anti-affinity)

The claim is a single atomic UPDATE; losing the race returns zero rows (I-4). The predicate enforces **all** applicable anti-affinity terms (I-10) at claim time — including the `merger` term, so the term declared in §5.5 is actually enforced here:

```sql
-- one atomic statement; 0 rows = lost the race, long-poll continues
UPDATE jobs
   SET state          = 'leased',
       bound_identity = $worker_identity,
       bound_model_family = $worker_model_family,
       lease_epoch    = lease_epoch + 1,          -- monotonic fence allocation
       lease_id       = $new_lease_id,
       lease_deadline = now() + $ttl,
       lease_hb_due   = now() + $hb_interval
 WHERE id    = $candidate_job
   AND state = 'ready'
   -- code_reviewer lease: exclude the eng_worker's identity AND model_family (I-10)
   AND NOT EXISTS (
        SELECT 1 FROM jobs sib
         WHERE sib.id = $candidate_job.eng_worker_job
           AND $candidate_role = 'code_reviewer'
           AND ( sib.bound_identity     = $worker_identity
              OR sib.bound_model_family = $worker_model_family ) )
   -- merger lease: exclude the bound code_reviewer's identity (I-10)
   AND NOT EXISTS (
        SELECT 1 FROM jobs sib
         WHERE sib.id = $candidate_job.code_reviewer_job
           AND $candidate_role = 'merger'
           AND sib.bound_identity = $worker_identity )
RETURNING *;
```

A **partial unique index** enforces *one active lease per job* structurally, so even a logic bug can't double-lease:

```sql
CREATE UNIQUE INDEX one_active_lease_per_job
    ON jobs (id)
 WHERE state IN ('leased','building','code_review','merging',
                 'merge_handoff','spec_authoring','spec_review');
```

A non-compliant worker simply **does not win the row** — the job stays `ready` and the `no_eligible_worker` timer (§6.6) arms. Independence is thus enforced *in the claim itself*.

#### 6.3.2 `lease_epoch` — allocation & scope

`lease_epoch` is a per-job monotonic counter incremented on **every** claim and every revocation. Its scope is exactly: **gating calls back into Flowbee** (T2). Every mutating worker call (`heartbeat`, `result`, `release`) carries `(lease_id, lease_epoch)`; Flowbee rejects with `409` any call whose epoch is not current.

```
Lease {
  lease_id        : ULID
  lease_epoch     : int          # the fence; bumped on claim AND on revocation
  identity        : principal     # bound; satisfies anti-affinity (I-10)
  model_family    : tag
  ttl             : duration      # = k × heartbeat_interval, k ≥ 3
  deadline        : ts            # ABSOLUTE cap — un-gameable floor (Rung-3)
  hb_due          : ts            # next heartbeat deadline
  state           : active | expired | revoked
}
```

What the epoch **cannot** do: stop a zombie from pushing a branch, triggering CI, or opening a PR — git/CI/GitHub never see the token (§3.5). That gap is closed structurally in §6.5, not by the fence.

#### 6.3.3 Heartbeat, TTL, and the clock

- **TTL = k × heartbeat_interval, k ≥ 3.** A missed heartbeat is a hint, not a kill.
- **The deadline is absolute.** Even a perfectly-heartbeating worker cannot hold a job past `lease.deadline` — the **Rung-3 (Flowbee-clock-only) absolute lease cap**, the un-gameable floor with full kill authority (§10).
- **Flowbee is the sole clock.** Lease expiry is decided by Flowbee's wall clock, never the worker's. The heartbeat carries `progress` hints (Rung-1) and receives a `directive: continue | cancel`; `cancel` is how a superseded/revoked job tells a still-running worker to stop.

#### 6.3.4 Partition ≠ stall

A worker that stops heartbeating may be **partitioned** (LAN/Tailscale drop) rather than **stalled** (T4). Lease expiry returns the job to `ready`, but the **reconnecting zombie is handled by fencing**: when the partitioned worker reconnects and POSTs, its now-stale epoch is rejected (`409`), and its side-effects are quarantined by epoch namespacing (§6.5). A *stall kill* (revocation as a liveness judgment) requires the **two-rung rule** (I-13, §10). Plain lease expiry (Rung-3 cap) is clock-truth and suffices to reclaim the row.

### 6.4 Lease lifecycle ↔ state transitions

| Lease event | Job transition | Counter / effect |
|---|---|---|
| atomic claim wins | `ready → leased` | epoch++; `bound_*` recorded |
| worker starts | `leased → building` / `…_authoring` / `…_review` | — |
| heartbeat OK | (no transition) | `hb_due` extended; `progress` recorded (Rung-1) |
| missed heartbeat (< Rung threshold) | (no transition) | hint only; never a kill alone |
| TTL expiry, `attempts < max` | active → `ready` | `attempts++`; epoch++; compensation (§6.5) |
| absolute deadline (Rung-3) | active → `ready` or `needs_human` | `attempts++`; epoch++; compensation |
| two-rung kill (I-13) | active → `needs_human` | revocation; epoch++; compensation; Rung-4 governor |
| `result` (valid epoch) | gate logic runs → next state | verdict **derived**, not taken (I-9) |
| `result` (stale epoch) | (rejected `409`) | zombie report quarantined; no state change |
| `release` (valid epoch) | active → `ready` | voluntary; `attempts++` |
| `superseded` observed | active → `superseded` | epoch++; compensation; re-arm |

A worker POSTing `result: succeeded` **never directly advances the gate**. It deposits a *claim* fenced by the epoch (§5.5); Flowbee independently reconciles Domain-B facts and only *then* mints the verdict and advances state (I-9).

### 6.5 Ack ≠ execution: epoch-namespaced side effects & compensation

The structural realization of T2 / I-12 inside the state machine:

1. **Epoch-namespaced refs.** A worker in `building` never pushes to a shared branch; it pushes to `refs/flowbee/<job_id>/epoch-<lease_epoch>`. On a *valid-epoch* `result`, Flowbee validates the epoch then **fast-forwards** the real branch from that ref. A stale epoch's ref is **orphaned, never promoted**.
2. **`(job, epoch)`-scoped CI.** CI status is keyed `(job, epoch)`. A zombie's checks, fired from a stale epoch's ref, can never satisfy a live job's gate.
3. **Flowbee opens the PR; the worker never supplies one.** The work-product carries `{patch | bundle, base_sha}` and **no PR field**. (Canonical PR-open trigger, §7.3.)
4. **Explicit compensation on revocation/expiry/supersede:**

```
compensate(job, dead_epoch):
    drop   refs/flowbee/<job>/epoch-<dead_epoch>          # orphan the zombie's work
    cancel (job, dead_epoch) CI                            # if cancellable
    if a draft PR was opened for this attempt:             # bounce/supersede only
        transition PR → draft  (project-OUT)               # never leave a ready zombie PR
    bump  epoch                                            # the reconnecting worker is now fenced (409)
```

5. **The terminal-SHA guard.** Once a job is `done` with a reconciled merge commit (I-3), no late or replayed event re-dispatches it.

### 6.6 Dependencies, the DAG & aging

Jobs form a DAG via `blocked_by`. A job is eligible for `ready` only when **all** predecessors are `done`. The scheduler is a **topological walk + priority + aging**: never offer a job whose `blocked_by` is unsatisfied (it sits in `blocked`); higher-priority jobs offered first; effective priority rises with `now() − enqueued_at` so no job starves; and a **`no_eligible_worker` alarm** fires when a `ready` job's match predicate — *including the §6.3.1 anti-affinity negations* — matches zero attested capabilities for too long (I-6). This is the surface where a **single-provider fleet** reveals itself: the build flow's `eng_worker.model_family ≠ code_reviewer.model_family` term is unsatisfiable, so the review job alarms rather than silently collapsing review independence.

### 6.7 Escalation: counters, cost ceiling, and the governor

Independent exhaustion conditions route a job to **`needs_human`** (non-terminal, I-6 + I-15):

| Trigger | Counter / meter | Ceiling | Notes |
|---|---|---|---|
| Lease failures (expiry/release/fail) | `attempts` | `max_attempts` | ports the home-grown "poison-guardian" |
| Gate reject loops | `bounces` | `max_bounces` | spec-review **and** code-review loops, counted separately per job |
| **Cost overrun** | `cost_tokens` / `$` | `cost_ceiling` per-job **and** per-flow | **I-15** — exceeding escalates, never silently overspends |
| Two-rung stall kill | (liveness) | — | **I-13**; revocation → `needs_human` via the **Rung-4 escalation governor** (anti-thrash) |

`needs_human` is the single chokepoint THE ONE DECISION (T1) operates on: **if the human merge gate STAYS**, the entire approved population funnels through a human checkpoint here (`self_merge` is policy-disabled, every approved job takes `handoff`→human), and the heavy content-integrity machinery (I-11) is deferrable; **if it COMES OFF**, `mergeable → merging|merge_handoff` proceeds unattended and the content-integrity gate (I-11), epoch-namespaced side effects (§6.5), and reconciled-verdict provenance (I-9) are the **actual MVP** guarding this transition. Only the policy on the `mergeable` exit changes — no restructuring. The Rung-4 governor also enforces anti-thrash across `needs_human ⇄ ready` cycles: a repeatedly killed-and-resumed job is held in `needs_human` rather than re-armed.

### 6.8 Where liveness rungs touch the state machine

| Rung | Authority over the state machine | Transition it can drive |
|---|---|---|
| **Rung-0** (worker-local supervisor) | **none** except locally-provable `agent_exited_zombie` | `agent_exited_zombie → failed` (free fast-path); else HINT only |
| **Rung-1** (corroboration-gated progress) | **never kills alone**; feeds `progress` via heartbeat | none directly; informs the two-rung rule |
| **Rung-2** (external diff-convergence oracle) | one of the two required rungs (I-13); **abstains when blind** | contributes to revocation → `needs_human`; extends tolerance on CI transitions |
| **Rung-3** (Flowbee-clock deadlines) | **full kill authority**; the absolute-lease-cap floor | TTL/absolute-deadline expiry → `ready` or `needs_human` |
| **Rung-4** (escalation governor) | converts repeated `stall_revocations` → `needs_human`; anti-thrash | `*→ needs_human`; holds against thrash |

The **core kill rule** (I-13) is enforced at the state-machine boundary: a transition to `needs_human` *justified as a stall kill* requires two independent rungs agreeing, at least one Rung-2 or Rung-3 — never two worker-reported rungs. Plain Rung-3 lease expiry needs no second rung. Full detail in §10.

---

## 7. Worker protocol & the provider-agnostic pull-loop

A worker is **an untrusted pair of hands**. It holds no GitHub credentials (R4), never speaks to GitHub, and learns nothing about Flowbee's state machine. It does exactly four things: register-and-attest, lease work, report progress and a result, release. Everything that makes Flowbee *correct* lives on the server side of this protocol. The wire contract is the trust boundary.

### 7.1 The thin loop (where a provider name is *allowed* to appear)

The reference worker is ~150 lines: a generic dispatcher around an **agent CLI** it knows nothing about. The provider appears in exactly two data values — a capability tag and a lens prompt path — and **never in control flow**.

```python
# flowbee-worker — the ENTIRE provider-specific surface is the two ENV values below.
AGENT_CMD   = env("FLOWBEE_AGENT_CMD")     # e.g. "codex exec" | "claude -p" | "aider"  ← data
MODEL_TAG   = env("FLOWBEE_MODEL_TAG")     # e.g. "model_family:codex"  ← a capability TAG
LENS_DIR    = env("FLOWBEE_LENS_DIR")      # persona prompt files; loaded per-role

def main():
    cred = load_mutual_auth()                          # mTLS client cert OR signed worker token (§7.6)
    wid  = register(cred, claimed=probe_local_caps())  # caps CLAIMED here, ATTESTED by server (§7.2)

    while True:                                         # ── the pull loop ──
        lease = long_poll_lease(wid, cred)             # blocks ≤30s; 204 → loop again
        if lease is None:
            continue
        # ── ONE-SHOT ISOLATION: fresh workspace + fresh agent process PER LEASE (§7.5) ──
        ws = provision_workspace(lease)                # worktree | bundle | scoped-read clone (§7.4)
        lens = read(f"{LENS_DIR}/{lease.job.role}.md") # the role's persona — only provider-flavored text
        agent = spawn(AGENT_CMD, cwd=ws, prompt=lease.job.instructions_ref, lens=lens)

        while agent.running():
            obs = collect_local_observations(agent, ws)        # tokens, tool_calls, edits, agent_health (§7.2)
            d = heartbeat(lease, obs, cred)                    # carries lease_epoch; server replies directive
            if d.directive == "cancel":                        # superseded / revoked / kill
                agent.terminate(); break
            sleep(lease.heartbeat_interval_s)

        wp = collect_work_product(ws, lease)           # PATCH+base_sha | bundle | prose verdict — NO pr (§7.3)
        post_result(lease, wp, cred)                   # idempotent; fenced by lease_epoch
        release(lease, cred)                           # explicit hand-back; server also reaps on TTL
        destroy_workspace(ws)                          # nothing survives the lease (§7.5)
```

Read off this loop: the agent CLI is a black box (`collect_work_product` extracts a *diff against a base SHA* or a *prose verdict*, never a parsed transcript); the loop never branches on model (`MODEL_TAG` is used only by the scheduler's match predicate; the §5.6 CI lint enforces no provider literal in any control position); isolation is per-lease (§7.5).

> **Mode A is the default.** This harness-driven loop (`flowbee work` spawning `FLOWBEE_AGENT_CMD` — e.g. `claude -p`, which is **subscription-covered** under an Anthropic plan, or `codex exec` — fresh-context + one-shot-per-lease) is the **recommended default** worker mode, and docs/config lead with it. The agent runs *under* Flowbee's supervision (Rung-0/§10), gets a fresh context per job (the anti-affinity substrate, §5.5), and holds **no GitHub credentials** (R4). **Mode B** (the thin `flowbee lease`/`submit` client, an operator wires their own agent loop) remains supported but is the advanced/optional path.

### 7.2 The five calls

Language-agnostic HTTP/JSON. **Mutual auth on every call** (§7.6). **Every mutating call carries `lease_epoch`** and is rejected `409 Conflict` if stale — fencing gates calls *back into Flowbee* and nothing else (§3.5). SSE is post-MVP; until then `lease` long-polls.

| Call | Method & path | Purpose | Fenced? | Idempotent? |
|---|---|---|:--:|:--:|
| **register** | `POST /v1/workers/register` | enroll identity, submit claimed caps → server **attests** | n/a (auth'd) | yes (re-register returns same `worker_id`) |
| **lease** | `GET /v1/lease` | long-poll for eligible work; **exactly-once** atomic claim | n/a | n/a (read) |
| **heartbeat** | `POST /v1/jobs/{job}/heartbeat` | prove liveness + feed the ladder; get a directive | **yes** | yes (latest wins) |
| **result** | `POST /v1/jobs/{job}/result` | submit the **work-product** | **yes** | **yes** (idempotency key) |
| **release** | `POST /v1/jobs/{job}/release` | hand the lease back explicitly | **yes** | yes |

#### register — claim, then *attest*

A worker advertises what it *claims*. Flowbee **never trusts the claim** — it probes/attests each capability before the worker is eligible for a matching job. ("Probed" = "attested" = the server-verified subset; canonical term **attested**.) Without attestation, an unverified worker advertises `role:code_reviewer` and rubber-stamps its own builds, recreating the arch-lottery as a capability-staleness lottery.

```jsonc
// → POST /v1/workers/register
{
  "identity": "mac-mini-1.codex",                 // Flowbee-enrolled principal; bound to the auth credential
  "host": "mac-mini-1",
  "claimed_capabilities": [
    "role:eng_worker", "role:code_reviewer",      // roles it OFFERS to fill (subject to anti-affinity at lease time)
    "model_family:codex",                         // ← the ONLY place a provider name appears (a TAG)
    "arch:arm64", "os:macos-15", "tool:docker", "tool:playwright"
  ],
  "max_concurrent_leases": 1                       // one-shot isolation default (§7.5)
}
// ← 200
{
  "worker_id": "w_7f3a",
  "lease_ttl_s": 300,
  "heartbeat_interval_s": 30,
  "attested_capabilities": [                       // SERVER-VERIFIED subset — only these gate matching
    "role:eng_worker", "role:code_reviewer",
    "model_family:codex", "arch:arm64", "os:macos-15", "tool:docker", "tool:playwright"
  ],
  "attestation_expires_at": "2026-06-15T18:00:00Z" // re-attest before this or the cap goes dormant
}
```

`identity` is unforgeable and bound to the credential (§7.6). The **attested** set — not the claimed set — is what the scheduler matches and what anti-affinity reasons over. Capabilities are re-attested periodically; an expired attestation makes a capability dormant rather than silently stale.

#### lease — long-poll, exactly-once

`GET /v1/lease` blocks ~30s, then competes for a job via the single atomic claim of §6.3.1 (0 rows = lost the race), backed by the partial unique index. The match predicate incorporates the job instance's anti-affinity negations (§5.5).

```jsonc
// → GET /v1/lease?worker_id=w_7f3a&role=code_reviewer   (role filter optional)
// ← 200  (work granted)
{
  "lease_id": "l_9c2",
  "lease_epoch": 42,                              // the fencing token for THIS lease (§3.5)
  "lease_ttl_s": 900,
  "lease_deadline": "2026-06-15T17:30:00Z",       // ABSOLUTE cap; Rung-3 floor (§10). Flowbee's clock only.
  "heartbeat_interval_s": 30,
  "job": {
    "job_id": "j_212",                            // canonical envelope field: job.job_id
    "kind": "build",                              // spec | build (exactly two)
    "role": "code_reviewer",
    "instructions_ref": "...",                    // what to do; lineage-aware (spec, issue)
    "context_policy": "diff_only",                // strip untrusted PR/issue prose (I-10) — server enforces
    "repo": {
      "provisioning": "worktree",                 // worktree | bundle | scoped_read  (§7.4)
      "base_sha": "9f1c…",                        // present for build flow; ABSENT for spec flow
      "mirror_path": "/srv/flowbee/mirror/russ.git",      // same-box only
      "fetch_ref": null, "read_credential": null          // populated for cross-box modes
    },
    "push_target": "refs/flowbee/j_212/epoch-42", // epoch-namespaced; worker pushes HERE, never to a branch
    "spec": null                                  // present ONLY for kind=spec jobs (see below)
  }
}
// ← 204  (nothing eligible right now — loop and long-poll again)
```

For a **`kind=spec`** job the same envelope is used (`job.job_id`, top-level lease fields); `repo.base_sha` is absent and an optional `spec` block carries the spec-only fields:

```jsonc
"spec": {
  "spec_ref": "refs/flowbee/spec/j_spec_8841",
  "spec_content_hash": "blake3:9c2f…",   // the artifact address the verdict will bind to (§11.5)
  "spec_version": 2,
  "spec_doc": "<full spec.md text @ v2>",
  "spec_diff": "<git diff v1..v2>",       // present iff version > 1 (re-review delta)
  "prior_verdicts": [ { "version": 1, "verdict": "changes_requested", "summary_hash": "…" } ]
}
```

#### heartbeat — `{continue|cancel}` + enriched liveness fields

A heartbeat proves the **worker loop** is alive; it says nothing about whether the **agent is converging** (T4). The draft's `progress:"..."` string is replaced with structured observations feeding the **lower, gameable rungs** — explicitly **hints**, because Rung-0/1 signals are worker-reported and may never kill alone (I-13).

```jsonc
// → POST /v1/jobs/j_212/heartbeat
{
  "lease_id": "l_9c2",
  "lease_epoch": 42,                              // fenced: stale epoch → 409, worker must stop
  "observations": {
    "agent_health": "ok",                         // ok|zombie|stdin_block|cpu_spin|oom|hung_child (Rung-0 HINT)
    "tokens_in": 41200, "tokens_out": 8800,       // Rung-1 progress accounting (gameable → corroboration-gated)
    "tool_calls": 37, "edits": 12, "checkpoints": 2,
    "cost_usd_so_far": 0.94,                       // feeds the per-job cost ceiling (I-15)
    "awaiting_input": false                        // true → fast-path cancel (MVP free path)
  }
}
// ← 200  (the directive is the ONLY authority here)
{ "directive": "continue" }
// ← 200  (superseded by a new SHA, revoked by a two-rung kill, or cancelled)
{ "directive": "cancel", "reason": "superseded_sha" }   // reason ∈ superseded_sha|revoked_stall|cancelled|epoch_stale
```

How the directive is decided (full ladder in §10): a locally-provable `agent_exited_zombie` → immediate `cancel`/`failed` (free fast-path); `awaiting_input: true` → `cancel` (free fast-path); a presumed **stall** never kills on these worker-reported fields alone — a `cancel` for `revoked_stall` requires **two independent rungs, at least one Rung-2 or Rung-3** (I-13). **Partition ≠ stall:** if heartbeats stop, Flowbee expires the lease on its own clock; the reconnecting zombie's next fenced call gets `409`/`epoch_stale` and self-terminates.

#### result — the work-product channel (see §7.3)

#### release — explicit hand-back

```jsonc
// → POST /v1/jobs/j_212/release
{ "lease_id": "l_9c2", "lease_epoch": 42, "disposition": "completed" }  // completed|abandoned
// ← 200 { "ok": true }
```

`release` is cooperative; the server *also* reaps on TTL, so a worker that crashes between `result` and `release` loses nothing. `abandoned` lets a worker voluntarily return work without burning an attempt as a failure.

### 7.3 The work-product channel (and the canonical PR-open trigger)

The draft's `result` carried `"pr": 2213`. **That field is removed.** "A PR exists / its number" is a **Domain-B fact GitHub owns** (§3.4) — a worker asserting it is the self-reported-success vector I-9 forbids. The worker returns a **work-product**; *Flowbee* opens the PR and stamps the number.

| Role / flow | `work_product.kind` | Payload | What Flowbee does with it |
|---|---|---|---|
| `eng_worker` (build) | `patch` | unified diff **+ `base_sha`** (or a pushed epoch ref) + declared `blast_radius` | validates epoch, fast-forwards the epoch ref → real branch, **opens the PR**, stamps the number |
| `spec_reviewer`, `code_reviewer` | `verdict` | `signed_off|changes_requested|approved` + reasoning + (review) disposition | records a **claim**, mints the tamper-evident sign-off only after reconciling Domain-B facts (I-9, §5.5) |
| `spec_author` | `spec_doc` | spec prose + draft issue bodies | Flowbee commits to `refs/flowbee/spec/<job>`, computes `spec_content_hash`; materializes issue(s) **after** the spec-review gate signs off (§11) |

**The single canonical PR-open trigger** (cited by §6.5, §8.2 — not restated as a divergent version): on a **valid-epoch `eng_worker` result**, Flowbee, as **one atomic step**, (1) validates the epoch, (2) fast-forwards `refs/flowbee/<job>/epoch-<n>` onto the real branch, (3) opens the PR, (4) stamps `pr_number` into the job's lineage, (5) transitions `building → review_pending`.

```jsonc
// → POST /v1/jobs/j_212/result        (eng_worker, build flow)
{
  "lease_id": "l_9c2",
  "lease_epoch": 42,                              // fenced — a stale epoch's result is rejected, never applied
  "idempotency_key": "j212-e42-b3d9",             // retried result returns the SAME response, no re-apply, no re-emit
  "work_product": {
    "kind": "patch",
    "base_sha": "9f1c…",                          // MUST match the lease's base
    "pushed_ref": "refs/flowbee/j_212/epoch-42",  // worker pushed here; Flowbee promotes only after epoch check
    "blast_radius": {                             // DECLARED scope — checked against the actual diff (I-11)
      "paths": ["api/users.go", "api/users_test.go"],
      "loc_added": 73, "loc_removed": 9,
      "touches_denylist": false                   // worker's claim; Flowbee re-derives it independently
    }
  },
  "status": "succeeded"                            // a HINT only — NOT the verdict (I-9)
  // NOTE: there is NO "pr" field. Flowbee owns PR existence/number (Domain B, §3.4).
}
// ← 200
{ "accepted": true, "job_state": "review_pending", "pr_number": 2213 }   // Flowbee opened & stamped it
```

```jsonc
// → POST /v1/jobs/j_390/result        (code_reviewer, build flow)
{
  "lease_id": "l_b71", "lease_epoch": 17, "idempotency_key": "j390-e17-rv1",
  "work_product": {
    "kind": "verdict",
    "verdict": "approved",
    "disposition": "self_merge",                  // self_merge | handoff — a REQUEST; policy may force handoff (§5.4)
    "reasoning": "Matches the spec's auth-flow acceptance criteria; standards clean."
  },
  "status": "succeeded"                            // a HINT; the sign-off is minted server-side post-reconcile
}
// ← 200 { "accepted": true, "job_state": "reconciling_verdict" }
```

The worker's `status` and `verdict` are recorded as **fenced claims**. They are *not* the sign-off. Flowbee independently reconciles `ci_green_at_head`, the PR's `(head_sha, base_sha)` pair, and the content-integrity gate (§9.2) **before** minting the tamper-evident, SHA-bound sign-off (§5.5, I-9, I-11). The strongest move a compromised worker has is to fabricate or withhold a verdict claim — which fails reconciliation and never becomes a sign-off.

### 7.4 Repo provisioning (two topologies)

A worker needs a **readable working tree** to run an agent; it must **never** hold an identity-token (R4, I-7). Because workers dial out (R2), the same wire protocol serves both topologies — only `repo.provisioning` differs.

```
 ┌─ SAME BOX (worker co-located with Flowbee) ───────────────────────────────────┐
 │   Flowbee keeps a shared local MIRROR:  /srv/flowbee/mirror/russ.git           │
 │   per-lease:  git worktree add <ws> <base_sha>     ← O(1), no network, no creds │
 └────────────────────────────────────────────────────────────────────────────────┘
 ┌─ CROSS BOX (worker on another machine: LAN / Tailscale) ──────────────────────┐
 │   (a) BUNDLE      — `git bundle` of base_sha served over the auth'd channel;    │
 │                     worker clones from it. No live creds. Best for air-gapped.  │
 │   (b) SCOPED-READ — a short-lived, READ-ONLY, repo-scoped credential to fetch   │
 │                     base_sha (a credential class the draft lacked). Never write,│
 │                     never the installation token. Expires with the lease.       │
 └────────────────────────────────────────────────────────────────────────────────┘
```

| Mode | When | What the worker gets | Write path | Credential blast radius |
|---|---|---|---|---|
| `worktree` | same box | a `git worktree` off the shared mirror at `base_sha` | local push to epoch ref; Flowbee promotes | **none** — no token at all |
| `bundle` | cross-box, no inbound repo access | a `git bundle` over the authenticated channel | result returned over the same channel | **none** — read-only data |
| `scoped_read` | cross-box, fetch acceptable | a short-lived **read-only, repo-scoped** credential | epoch ref pushed back via scoped channel | read-only, single-repo, lease-lived |

Invariant across all three: **the worker never receives a credential that can write `main` or read the installation identity.** Same-box gets *zero* credentials. All promotion (epoch ref → branch → PR → merge) is performed by Flowbee under its single installation identity (§9.4, I-14). The same applies to the Flowbee-owned spec ref `refs/flowbee/spec/<job>` — a `spec_author` returns prose; Flowbee commits it (§11).

### 7.5 Long-lived loop vs. one-shot-per-job isolation

The worker loop is **long-lived**; the *agent and its workspace are one-shot per lease*.

```
  worker process (long-lived) ──┬── lease j_212 → fresh worktree W1 → fresh agent A1 → result → destroy W1, reap A1
                                ├── lease j_390 → fresh worktree W2 → fresh agent A2 → result → destroy W2, reap A2
                                └── … (the loop persists; nothing crosses the dashed boundaries)
                                      ▲ registration + attestation done ONCE, reused across leases
```

Per-lease isolation is non-negotiable: no cross-job contamination (fresh worktree at the lease's `base_sha`, clean env); clean fencing semantics (revocation terminates exactly one agent, destroys exactly one workspace, so compensation is well-defined); bounded blast radius for a compromised agent (it lives and dies inside one workspace with no credentials and no path to GitHub). The cost is per-lease spawn overhead — cheap and local, versus contamination which is expensive and silent.

### 7.6 Mutual auth & fencing (the trust boundary)

Every call is mutually authenticated — **mTLS client certs or signed per-worker tokens** — against a Flowbee **allowlist of enrolled identities**. Unregistered callers are rejected; `worker_id`/`identity` are unforgeable and bound to the credential. Fencing layers on top and is orthogonal:

- **Auth** answers *"are you an enrolled worker?"* (a standing credential).
- **Fencing** (`lease_epoch`) answers *"are you the CURRENT holder of THIS lease?"* (a per-lease token, bumped on every grant/reassignment).

This is the mechanism behind T2: fencing gives **exactly-once acknowledgement** *into Flowbee* — it does **not** reach git/CI/GitHub, which is why epoch-namespaced refs, `(job, epoch)`-gated CI, and explicit compensation (§3.5, I-12) carry the idempotency fencing alone cannot.

### 7.7 What the protocol deliberately does *not* let a worker do

A worker **cannot**: assert a PR number or PR existence (no PR field — Domain B); write a verdict/sign-off (it submits a *claim*; Flowbee mints from reconciled state — I-9); push to a real branch (only to `refs/flowbee/<job>/epoch-<n>`, promoted by Flowbee — I-7/I-12); hold any GitHub credential that can write `main` or read the installation identity (§7.4, I-14); kill a job by self-report (worker observations are *hints*; a stall kill needs two independent rungs — I-13); be matched on an *unattested* capability (attest-or-dormant, §7.2); review a job it built (anti-affinity is a lease-time scheduler constraint, §5.5, I-10). The worker is powerful locally and powerless globally — the containment posture R4 and §2.4 demand.

---

## 8. GitHub integration (reconcile-IN + project-OUT)

This operationalizes §3. Flowbee is the **sole** GitHub caller (R4), so the single-token storm that wedged the old pipeline structurally cannot recur. The whole integration is **two loops and exactly two**, with a hard asymmetry: reconcile-IN may write only the Domain-B fact-fields into the store (I-1); project-OUT is the only writer of GitHub state derived from Domain A. Neither crosses into the other's column of §3.4.

### 8.1 reconcile-IN — ground truth for the GitHub-owned facts

The **only** authority for Domain B. It exists because webhooks are lossy, replayable, forgeable, and silently dropped — so "does this PR exist, is CI green at this SHA, is it merged" is established by *Flowbee pulling it*.

#### 8.1.1 The batched GraphQL sweep

One batched query (the lineal descendant of the old `reconcile.go`) on a low-frequency cadence — **every 2–5 min**, **on boot**, **on gap-detection**. One snapshot of the whole board is cheap; the old storm came from *N redundant per-PR loops on one token*, not a periodic snapshot. Webhooks tighten effective freshness via targeted refetch (§8.1.3); the sweep is the floor.

```graphql
query BoardSweep($owner:String!, $repo:String!, $prCursor:String, $issueCursor:String) {
  repository(owner:$owner, name:$repo) {
    pullRequests(first:50, after:$prCursor, states:[OPEN, MERGED], orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number  updatedAt  isDraft  merged  mergedAt
        headRefOid          # ── Domain-B: head SHA
        baseRefOid          # ── Domain-B: base SHA
        mergeCommit { oid } # ── Domain-B: merge commit SHA
        mergeStateStatus    # CLEAN | BEHIND | BLOCKED | DIRTY | UNSTABLE  (hint)
        commits(last:1) { nodes { commit {
          statusCheckRollup { state }   # ── Domain-B: CI rollup @ head
        }}}
        labels(first:20) { nodes { name } }   # read to DETECT drift on Flowbee-owned renderings
      }
    }
    issues(first:50, after:$issueCursor, states:[OPEN], orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { number updatedAt labels(first:20){ nodes{ name } } }
    }
  }
  rateLimit { limit cost remaining resetAt }   # ── Domain-B: budget, every sweep self-meters
}
```

| Captured field | GraphQL source | Drives |
|---|---|---|
| PR exists / number | `pullRequests.nodes[].number` | job↔PR binding; supersession |
| head / base SHA | `headRefOid` / `baseRefOid` | SHA-pair gate (I-5), supersession (I-3), epoch validation |
| CI status rollup @ SHA | `statusCheckRollup.state` | code-review gate `ci_green_at_head` (I-9) |
| merged? / merge commit | `merged` / `mergeCommit.oid` | terminal-SHA guard (I-3); merge reconciliation |
| `rateLimit.remaining` | `rateLimit { remaining resetAt }` | project-OUT backoff + cost meter (I-15) |

Labels are fetched only so reconcile-IN can *detect drift* on Flowbee-owned renderings and hand the correction to project-OUT (§8.2.3). Reading a label is observation; the label is still Flowbee's output.

#### 8.1.2 Drift correction (and its hard limit)

The sweep diffs fetched Domain-B facts against the store and corrects drift — **but only within Domain B.** PR/CI/merge facts changed → write the new fact, then evaluate consequences. A Flowbee-owned rendering drifted (a human flipped a `flowbee:` label) → record *that a drift exists* and enqueue a project-OUT reassertion (§8.2.3); the human edit is **not** input. What reconcile-IN may **never** do: touch a stage, verdict, lens, lease, or any Domain-A field. If a sweep "disagrees" about whether a spec was signed off, Flowbee is right and there is nothing to reconcile — GitHub has no such fact.

#### 8.1.3 Webhooks are HINTS → targeted refetch

```
inbound webhook ──▶ [1] HMAC-verify X-Hub-Signature-256   (reject unsigned: internet-reachable)  ── I-2
                ──▶ [2] dedupe on X-GitHub-Delivery        (durable inbox; replay-safe)           ── I-2
                ──▶ [3] write-ahead to inbox BEFORE acting (crash-replay)                          ── I-2
                ──▶ [4] enqueue a TARGETED refetch of (owner,repo,#N) via the §8.1.1 fragment
                ──▶ [5] update the delivery high-water-mark (gap detection, §8.1.4)
```

Subscribed events (`pull_request`, `pull_request_review`, `check_run`, `check_suite`, `workflow_run`, `status`, `issue_comment`, `issues`, `push`) are all treated identically as *refetch hints*. A webhook and a sweep converge to the *same* reconciled fact through the *same* code path. The refetch is gated by the SHA-monotonic guard (§8.1.5), so a **forged or replayed webhook cannot fast-track an unreviewed merge**: the worst a forged `pull_request_review: approved` can do is trigger a refetch that reads the *real* (un-approved) state. The verdict was never GitHub's to give (I-9); the webhook is a doorbell.

#### 8.1.4 Gap detection

Two mechanisms catch dropped deliveries: a **delivery high-water-mark** (a discontinuity or a configurable silence window with known in-flight work forces an immediate full sweep) and **the floor sweep itself** (even with zero webhooks ever delivered, the 2–5 min sweep reconciles everything). Webhooks only *tighten* freshness; their total loss degrades latency, never correctness.

#### 8.1.5 SHA-monotonic gating + terminal-SHA guard (I-3)

Every ingested fact — sweep or refetch — passes two guards before it can move a job:

1. **SHA-monotonic gate.** Ignore any event whose `headRefOid`/`updatedAt` is older than recorded. Late/out-of-order deliveries cannot rewind state.
2. **Terminal-SHA guard.** A job whose PR is `merged` is **frozen**: no event re-dispatches it. The merge commit SHA is the terminal Domain-B fact; once recorded, the job is immutable against re-arming. This closes the double-merge failure at ingestion, before any flow logic runs.

A base/head SHA **move** (not a settle) is the third case: it **supersedes** any Domain-A verdict bound to the old pair (I-5) and re-arms `code_review` + CI (the `superseded` state, §6.2.4).

### 8.2 project-OUT — render Domain-A state via the outbox

The **only** writer to GitHub (R4). Each desired side-effect is an outbox row, written transactionally with the state change that motivated it, then drained by a single sender that owns the installation identity, the dedupe key, and the backoff.

#### 8.2.1 The mapping table: Flowbee state → GitHub rendering → action

Every action is keyed `(job_id, action, head_sha)` for idempotent dedupe (§8.2.2).

| Flowbee state element (Domain A) | GitHub rendering | Outbox action | Triggered when |
|---|---|---|---|
| `spec_review: signed_off` → issues materialized | issue(s) created | `issues.create` | spec flow `materialize_issues` (§11) |
| Stage = `building` | label `flowbee:building` | `labels.set` (replace `flowbee:*`) | `eng_worker` lease granted |
| `eng_worker` produced patch | **PR opened** (Flowbee opens it; stamps #) | `pulls.create` (draft) from Flowbee-promoted branch | the canonical PR-open trigger (§7.3) |
| Stage = `code_review` | label `flowbee:code-review`; PR draft→ready | `labels.set` + `pulls.update {draft:false}` | `code_review` lease granted |
| Code-review verdict = `approved` | status check `flowbee/review-valid@SHA` = success | `checks.create` | reconciled sign-off minted (§5.5, I-9) |
| Code-review verdict = `changes_requested` | label `flowbee:changes-requested` + review comment | `labels.set` + `pulls.comment` | bounce decision (`max_bounces`) |
| Disposition self_merge OR `merger` ready | PR **enqueued to merge queue** | `mergeQueue.enqueue` (§8.5) | gate clear + `review-valid` present + §5.4 predicate |
| Reconciled `merged: true` | label `flowbee:merged`; lineage closed | `labels.set` | reconcile-IN confirms terminal fact |
| `needs_human` (denylist / max bounces / stall) | label `flowbee:needs-human` + comment | `labels.set` + `pulls.comment` | I-6 / I-11 / I-13 escalation |
| Cost ceiling exceeded (I-15) | label `flowbee:over-budget` + comment | `labels.set` + `pulls.comment` | per-job/per-flow meter trips |

Two enforced rules: **Flowbee opens the PR and stamps the number** (the worker never supplies a PR field); and **`flowbee/review-valid@SHA` is a Flowbee-emitted, GitHub-re-evaluated status check** (the merge-queue fix, §8.5). Note `mergeQueue.enqueue` is how *both* merge arms physically merge (§5.4) — workers never call GitHub.

#### 8.2.2 Outbox semantics: `(job_id, action, head_sha)` dedupe

The idempotency backbone and the structural answer to "ack ≠ execution" on the GitHub side. **Transactional enqueue:** the outbox row is written in the *same* SQLite transaction as the Domain-A state change (§12.3); there is no window where Flowbee believes it labeled a PR it never did. **At-least-once send, exactly-once *effect*:** the sender may retry freely; combined with the key and observed GitHub state from the last reconcile, a re-sent `labels.set@SHA` or a duplicated `mergeQueue.enqueue` for the same `(job, head_sha)` collapses to one. **SHA in the key is load-bearing:** when the SHA moves (supersession), stale outbox rows are abandoned and fresh ones enqueued for the new SHA — the same mechanism that voids the sign-off voids its pending renderings.

#### 8.2.3 Reasserting drifted renderings

When reconcile-IN observes a Flowbee-owned rendering drifted, it enqueues a project-OUT reassertion: project-OUT re-renders the correct value. The label is *output*; a human edit is drift, corrected on the next drain. **Exception:** ADOPT-quiesced objects (I-16) are not reasserted until opted in (§12.7).

#### 8.2.4 Backoff, `Retry-After`, and the outbound concurrency cap

The sender honors GitHub's flow control as a first-class input. **Primary rate limit:** drains adaptively against `rateLimit.remaining`/`resetAt`; as the budget tails off it widens spacing and prioritizes correctness-critical actions (merge, status check) over cosmetic ones (labels). **`Retry-After` (secondary/abuse limit):** triggered by bursty concurrent writes independent of the hourly budget; the sender treats it as authoritative and parks the *whole* outbox for the stated duration. **Outbound concurrency cap:** the most effective abuse-limit defense is **serializing outbound writes** — one in-flight GitHub mutation at a time (small bounded read pool), jittered. Because there is exactly *one* sender owning *one* identity (§9.4), this is trivial to enforce with no second writer to race it.

### 8.3 Identity: one outbound GitHub identity — PAT for a single operator, App for multi-repo (I-14)

> **Reconciled.** The pre-build draft mandated a GitHub App installation token as *the* identity. The built reality and the resolved posture: **Flowbee owns actor identity, and the credential is sized to the deployment.** For the **single-operator, single-repo** case (the v1 default), a **fine-grained, repo-scoped Personal Access Token (PAT)** — or a `gh` token — is **sufficient and is the recommended default**: because Flowbee is reconcile-first (one batched sweep every 2–5 min, workers make *zero* GitHub calls), the 5k-requests/hr PAT budget is **never the bottleneck**, so the App's larger bucket buys nothing at this scale. A **GitHub App** is the right identity only at **org-scale / multi-repo / OSS-distribution**, where per-repo installation and a larger bucket matter. The invariant (I-14) is unchanged in substance — **one outbound identity, no multi-account pool** — only the credential *form* is now deployment-sized.

Outbound identity resolves to **a single GitHub identity** — one ToS-clean bucket. The draft's multi-PAT "multiply the buckets" *pool* (rotating several accounts to multiply the budget) is **dropped**; a *single* fine-grained repo-scoped PAT is not a pool and is the single-operator default.

#### 8.3.1 Why the multi-PAT pool is dropped — the ToS constraint

Rotating multiple PATs/bot accounts to *multiply* the budget is, in substance, evading GitHub's rate limits by sock-puppeting accounts. **GitHub's Terms of Service prohibit using multiple accounts to circumvent platform restrictions, including rate limits.** The failure mode is not "slightly against the rules" — it is account/installation suspension, which takes the *whole* control plane offline. A rate-limit *outage* wedges Flowbee until the window resets and is self-healing; a ToS *suspension* wedges it indefinitely and is not. **The ToS constraint is a first-class design input, not a tuning knob** (I-14): the budget is whatever one installation legitimately provides, and the architecture is sized to live within it.

#### 8.3.2 What the single identity gives, structurally (PAT or App)

**One legitimate bucket** — for a single operator, a fine-grained repo-scoped **PAT** (the recommended default); at org-scale, a GitHub **App** installation token sized by repo/installation. Either way: **no sock-puppets, one caller.** **Least privilege** — the credential is scoped to only contents, pull requests, issues, checks, statuses; workers never receive it (R4, I-7); git writes Flowbee performs use ephemeral per-job scoped credentials (a short-lived deploy key, §9.4), never the long-lived token handed outward. **It IS the single caller** — "one identity" and "one caller" are the same fact, which is why the outbound concurrency cap (§8.2.4) and the dedupe key (§8.2.2) are enforceable. **Flowbee owns actor identity:** every outbound GitHub action is attributed to this one Flowbee identity, which is *distinct* from any human author — the property branch protection's "review from an identity distinct from the author" leans on (§9.6).

#### 8.3.3 The rate-limit math (why one bucket is enough)

The old pipeline exhausted its token not because the *budget* was small but because ≈5 loops plus every worker poll hammered it, **three redundantly re-polling `statusCheckRollup`**. Flowbee deletes the demand:

| Cost driver (old) | Old behavior | Flowbee behavior | Cost/hr |
|---|---|---|---|
| `statusCheckRollup` polls | 3 loops, redundant, per-PR | folded into **one** sweep field | shared, ~0 marginal |
| Worker GitHub calls | every worker poll | **zero** — workers never call GitHub (R4) | **0** |
| Board read | N per-PR loops on one token | **1 batched GraphQL sweep** | ~1 query / 2–5 min |
| Outbound writes | scattered, unbudgeted | one paced outbox, ≤1 in-flight | bounded by need |

A batched sweep is ~1 GraphQL request every 2–5 min ≈ 12–30 requests/hr for the entire board. Against an installation bucket measured in thousands of points/hr, steady-state utilization sits in the low single-digit-percent range. **First measure `rateLimit.remaining` for a week** (the T1 lean-branch instruction): under this design the meter should barely move, and that measurement is what licenses removing any remaining interim throttles.

### 8.4 (reserved) — see §13.4 for the interim rate-limit patch

The interim, scripts-only rate-limit patch (deployable before Flowbee exists) is defined once in **§13.4** to avoid duplication; it is cited from here by reference.

### 8.5 The merge path: serialized, with the integrated-head review-revalidation fix

Merges are the single most dangerous side-effect. Both merge arms (§5.4) merge by **Flowbee enqueuing the PR to GitHub's native merge queue** (`mergeQueue.enqueue`); the merge commit is reconciled back. Workers never call GitHub.

#### 8.5.1 The merge-queue blind spot

GitHub's merge queue builds an **integrated head** (the PR rebased/batched onto the latest base, possibly atop other queued PRs) and **re-runs CI** at that integrated SHA — but it does **not** re-run *review*. So a PR approved at head `A`/base `B` can merge at an integrated head `A'`/`B'` that **no reviewer ever saw**. CI-green at `A'` is necessary, not sufficient; the review-valid fact must also hold at `A'`.

#### 8.5.2 v1 default: batch-size-1 (no custom check)

**v1 ships merge-queue batch-size-1.** With batch size 1, the integrated head differs from the reviewed head *only* when the base moved (no sibling PRs batched in), which supersession already catches and re-arms (§8.1.5) before the merge runs. Batch-size-1 trades merge throughput for the guarantee that no un-reviewed integrated head can merge, and it requires **no custom status check**. This is the v1 default per §13.

#### 8.5.3 v1.1 path: a Flowbee-controlled `review-valid` check re-evaluated at the integrated SHA

For **batch-size > 1** (v1.1), Flowbee emits a status check **`flowbee/review-valid@SHA`** that the merge queue is configured to require and that GitHub re-evaluates at the integrated head:

```
   reviewed head A/B ── gate clears ──▶ Flowbee mints sign-off (SHA-bound to A/B, I-9)
                                         ▼  project-OUT: checks.create flowbee/review-valid = success @ A
   merge queue builds integrated head A'/B'  ──re-runs CI──▶ CI green @ A'
                                         ▼  GitHub re-evaluates required check flowbee/review-valid @ A'
                          ┌──────────────┴───────────────┐
                  A' == reviewed SHA pair          A' != reviewed SHA pair
                  → check = success                → check = FAILURE/pending
                  → merge proceeds                 → merge BLOCKED; job re-arms code_review at A'
```

Flowbee sets `review-valid` to success **only for the exact `(head_sha, base_sha)` it reconciled and signed off** (I-9); for any other SHA it is failure/pending. A failed re-evaluation is a **supersession event**: the job re-arms `code_review` at the new integrated SHA, a fresh reconciled sign-off is minted for `A'`, project-OUT re-emits `review-valid@A'`, and the PR re-enters the queue. This is why the `merger` stage exists as a distinct seam (§5.4): the integrated-head re-check is precisely where a dedicated integrator's re-validation lands.

#### 8.5.4 Why never a direct Flowbee merge at the reviewed SHA

A direct merge the moment the gate clears would merge at the **reviewed** SHA without re-checking CI at the **post-rebase** SHA — the same blind spot. If Flowbee ever performs a direct merge (degraded mode, no queue), it **must** re-verify CI-green and `review-valid` at the *post-rebase* head — i.e. reimplement what the queue + check already do. The queue is the right tool; the direct path inherits all the same SHA-pair obligations.

#### 8.5.5 Server-side branch protection as the backstop (I-8)

All of the above is Flowbee-side logic, backed by **server-side branch protection on `main`** that holds even if Flowbee has a bug: no direct/force push, required status checks (including `flowbee/review-valid` when in use), and **required review from an identity distinct from the author**. Flowbee logic is the policy; branch protection is the law (§9.6).

---

## 9. Security model

> Threat model in one sentence: **a provider-agnostic agent, running under `--dangerously-bypass-permissions` with repo write and broad local privilege, reading attacker-controlled issue/PR prose, on a box Flowbee does not own.** Everything here exists because that sentence is true on day one.

The worker is **untrusted, and so is everything it returns** (T3, I-7, I-9–I-11). Flowbee's job is not to believe the worker; it is to *contain* it and to *derive* truth independently. No single compromise — a prompt-injected agent, a forged `worker_id`, a zombie that survived revocation, a hostile worker host — can corrupt `main`, manufacture a merge, or collapse review independence.

### 9.1 Attacker model & assets

| Asset to protect | Primary threat | Defending layer |
|---|---|---|
| `main` (and protected branches) | rogue/injected agent force-pushes or merges unreviewed code | branch protection (§9.6) + Flowbee-only git writes (§9.4) + content gate (§9.2) |
| The merge decision | worker self-reports `succeeded` → spurious merge | reconciled-verdict sign-off (§5.5, I-9) |
| Review independence | builder reviews own work / same model rubber-stamps | identity+family+lens anti-affinity (§5.5), enforced at lease time |
| Installation identity / token | exfiltration → ToS-violating use, rate-budget theft | credential classes (§9.4): the installation token never leaves Flowbee |
| Secrets in the repo & supply chain | diff edits CI config, lockfile postinstall, Dockerfile, secrets | path denylist + static checks (§9.2) |
| Confidentiality of repo & prompts | hostile worker host exfiltrates over its own egress | named, bounded (§9.7) — v1 bounds blast radius, does not yet prevent egress |
| The control plane itself | forged worker registers, leases context, POSTs forged success | mutual auth + enrolled-identity allowlist (§9.5) |

Two attacker entry points are *normal operation*, not edge cases: **prompt injection via untrusted prose** (issue bodies, PR descriptions, review threads are attacker-controllable; an agent reading them reads attacker instructions — which is why reviewer context is *stripped*, §9.3, and no LLM verdict alone clears the merge gate, §9.2); and **the agent runs with permission bypass** (we do not sandbox the agent's *intent*; we make its *output* untrusted data that must survive deterministic checks, and its *credentials* too weak to do damage, §9.4).

### 9.2 Content integrity: the returned diff is untrusted data (I-11)

A worker returns a `{patch | bundle, base_sha}` work-product (§7.3) — never a PR number, never a merge. Before that diff is **auto-merge-eligible** (the §5.4 predicate), it must clear three deterministic, non-LLM gates. No LLM verdict substitutes for any.

**(a) Path denylist → forced human gate.** Any hit forces the `handoff` arm and a human gate; an LLM `code_reviewer` may **not** wave it through, and `self_merge` is denied:

| Denylisted path class | Why it is load-bearing |
|---|---|
| `.github/workflows/**`, CI config | edits here change what "CI green" *means* — a diff that weakens its own gate |
| lockfiles + `postinstall`/lifecycle scripts (`package-lock.json`, `*.lock`, `preinstall`/`postinstall`) | arbitrary code execution at install time on every box that pulls the branch |
| `Dockerfile`, base-image refs, devcontainer | supply-chain substitution; runs as root in CI |
| secrets / `.env` / credential material / signing keys | exfiltration or substitution |
| Flowbee's own source & config (flows, lenses, the denylist itself) | the agent editing the rules it is judged by — privilege escalation against the orchestrator |

The denylist is itself in the protected set: a diff that edits the denylist is the canonical privilege-escalation attempt and is always a human gate.

**(b) Declared blast-radius, checked against reality.** The `eng_worker` must `declare: blast_radius` (§7.3). Flowbee compares *declared* against *actual* touched paths. A mismatch — the diff touches more than it declared — is a tamper signal treated as a denylist-class event (forced handoff/human gate). Declaration is not trusted; it is a *commitment verified against the diff*, and disagreement is itself evidence.

**(c) Deterministic non-LLM static checks.** The patch applies cleanly at `base_sha`; it parses/compiles where cheap to prove; secret-scanning (entropy + known-pattern) passes; no binary blobs outside an allowlist; size/oversize bounds hold. Mechanical and un-gameable by narration.

Only a diff that is **denylist-clear ∧ blast-radius-consistent ∧ static-checks-green** is *eligible* for `self_merge` (and only then if §14 has removed the human gate). Everything else takes `handoff`. Under **Branch A** these are deferrable (a human gates anyway); under **Branch B** they are *the* MVP safety boundary.

### 9.3 Independence & reconciled sign-offs

**Anti-affinity, enforced at lease time (I-10).** The canonical four terms (§5.5) are hard scheduler constraints on the active job instance; a lease that would violate any is not granted. The `model_family` term means a single-provider fleet **cannot** satisfy the build flow and surfaces a `no_eligible_worker` alarm — the system structurally uses ≥2 providers on any reviewed job. Heterogeneity is the defense.

**Strip untrusted prose from the reviewer's context.** The `code_reviewer` runs with `context: diff_only` (§5.3). PR descriptions, issue bodies, and review-thread prose are **removed**. The reviewer judges *the diff, not the attacker's narration*, defeating the "PR body contains injection telling the reviewer to approve" attack. The lineage spec the reviewer *does* receive is Flowbee-owned process state, authored upstream, not attacker-controlled at review time.

**Sign-offs come from reconciled GitHub state, never self-report (I-9).** The full rule and its mint sequence are canonical in §5.5; cited here as the heart of the gate. The strongest move a compromised worker has is to *withhold or fabricate a verdict claim* — which fails reconciliation and never becomes a sign-off. It cannot manufacture a merge by lying.

### 9.4 Credential classes

The single most effective containment is **not handing the worker anything dangerous.** Three credential classes with strictly disjoint reach:

| Class | Held by | Scope / lifetime | Reach if stolen | Never |
|---|---|---|---|---|
| **Installation token** — the single GitHub App installation token (I-14) | **Flowbee only**, in-process | org-installation scope, long-lived, rotated by Flowbee | full outbound GitHub identity | …leaves the Flowbee process. Not in a lease, not in worker env, not in a worktree. |
| **Per-job WRITE credential** | **Flowbee exercises it itself**; if a worker must push, a *short-lived scoped deploy key* | single repo, single job, minutes-long TTL, push only to `refs/flowbee/<job>/epoch-<n>` | push to one orphan ref Flowbee fast-forwards only post-epoch-validation (I-12) | …grant write to `main` or shared branches; …outlive the lease epoch. |
| **READ / fetch credential** (cross-box provisioning) | enrolled worker, for repo materialization | single repo, read-only, short TTL | clone/fetch the repo it was already going to build | …write anything. (A credential *class the draft lacked*, R5.) |

Flowbee performs all consequential git writes (push to real branches, open PR, enqueue merge) itself with the installation token (I-7). A worker that must push gets only a deploy key scoped to *one epoch-namespaced ref* — so a zombie that survived revocation can at most pollute an orphan ref Flowbee will never promote. The worker never receives the installation token, so an exfiltrating host (§9.7) cannot steal Flowbee's GitHub identity. The READ class exists because cross-box provisioning needs repo bytes on a box that must hold *no* write authority — same-box workers skip it entirely (worktree off the local mirror).

**ToS constraint, stated (I-14):** the multi-PAT "multiply the buckets" pool is **dropped** — a ToS-suspension vector. One identity also means one auditable caller (R4): the security and rate-limit stories share a root.

#### 9.4.1 Mutual auth & enrolled-identity allowlist

Workers dial *out* (R2), so the wire is **mutually authenticated**: mTLS or signed per-worker tokens against a Flowbee allowlist. An unregistered caller is rejected; `worker_id` is unforgeable and bound to the credential. The §5.5 reconciled-sign-off rule blunts a forged *verdict*, but mutual auth is what stops an unenrolled box from leasing job context in the first place. **Capabilities are probed/attested, not self-declared** (original draft §8; canonical term **attested**, §7.2) and re-attested periodically — otherwise an unverified worker advertises `role:code_reviewer` and rubber-stamps its own builds.

### 9.5 Topology is a security trade

Because workers dial out, the *protocol* is identical same-box or cross-box — but the *isolation posture is not*, and this is an explicit trade.

| Posture | Isolation | Notes |
|---|---|---|
| **Same-box co-location** | **Weaker.** The worker (and its bypass-mode agent) shares a kernel, filesystem, and loopback with Flowbee and its SQLite store. A compromised agent that escapes its cwd is *adjacent to the installation token's process* and the system of record. | Cheapest, lowest latency; fine for a trusted single-operator dev box. Provision via `git worktree`; run the worker under a separate OS user with filesystem confinement at minimum. |
| **Cross-box over Tailscale/WireGuard** | **Stronger, and reusable as the auth substrate.** WireGuard gives transport encryption + cryptographic **node identity**; Tailscale **ACLs** restrict which nodes may reach Flowbee's port. A compromised host is across a network boundary from the token and the store. | The node identity folds into §9.4.1 mutual auth — the Tailscale node key becomes part of "this is an enrolled worker." Preferred for any worker you would not run as yourself. |

The non-obvious point: **moving a worker further away makes it safer.** Co-location optimizes latency at the cost of blast radius; the Tailscale path converts "shares a kernel with my secrets" into "an ACL'd, cryptographically-named node across a WireGuard boundary."

### 9.6 Branch protection: a *structural* gate, not the reviewer≠author arbiter (I-8)

> **Reframed (reconciled).** Branch protection is a **structural** server-side gate — *"nothing reaches `main` except via Flowbee's check; no force-push, no direct push."* It is **not** the thing that decides reviewer-≠-author independence. **Flowbee owns actor identity and enforces anti-affinity** (I-10, §5.5, §9.3) at lease time: `eng_worker.identity != code_reviewer.identity` etc. is a Flowbee scheduler constraint, derived from Flowbee-owned process state, not something GitHub's "require review from someone other than the author" can express — because every Flowbee-driven commit and review is attributed to the *single* Flowbee identity (§8.3), so to GitHub author and reviewer look like the same actor. GitHub's branch protection therefore cannot be the arbiter of review independence; **Flowbee is.** Branch protection's job is the *structural* one only.

Everything above is Flowbee logic, and Flowbee can have bugs. **Server-side branch protection on `main` is the structural backstop that does not depend on Flowbee being correct:** no direct/force push; required status checks (including `flowbee/review-valid@SHA` when batch-size>1, §8.5); merges only through the protected path Flowbee drives. This is defense in depth with a deliberately *different trust root*: a rogue worker, a forged lease, **or a Flowbee bug** still cannot force-push or directly push to `main`. The **reviewer-≠-author** guarantee is supplied by **Flowbee's lease-time anti-affinity (I-10)**, not by GitHub — because Flowbee owns actor identity, GitHub's author/reviewer distinction degenerates to one identity. Flowbee complements branch protection; it never assumes it away, and it never delegates *independence* to it.

### 9.7 The worker host as a hostile execution environment

The honest gap, named not hidden. The worker host runs an agent with permission bypass and (for a build) a clone of your source plus the prompts and lineage Flowbee sent. **That host has its own network egress, which Flowbee does not control.** A malicious or compromised host can **exfiltrate** repo contents, prompts, and any data the agent touches over its *own* outbound connections. No GitHub-side discipline prevents a box you don't own from copying bytes it legitimately received.

- **Does (bounds blast radius):** the worker holds *no* installation token and at most a one-ref, one-epoch write key (§9.4), so exfiltration cannot escalate into write authority over `main` or theft of Flowbee's GitHub identity. The damage ceiling is "the attacker learned the contents of a repo they were already trusted to build," not "the attacker controls the merge pipeline or the org token."
- **Does (cryptographic provenance):** cross-box workers are ACL'd, named WireGuard nodes (§9.5) — a hostile host is *identified and revocable*, not anonymous.
- **Does NOT (v1):** prevent egress. No egress firewall, no network-namespace jail, no data-diode in v1. Confidentiality against a hostile *worker host* is **not** a v1 guarantee.

Mitigation is operational and explicit: **run cross-box workers only on hardware you would trust with the repo's source**, prefer the Tailscale posture, and treat the READ credential's scope as the confidentiality boundary it is. Egress confinement (per-worker network namespace, allowlisted egress, or an outbound proxy pinning the agent's traffic) is the **first item of post-v1 hardening**. v1 makes the worker host *unable to escalate*; it does not make it *unable to leak*. **If §14 removes the human merge gate, §9.7's egress gap moves from "named post-v1 work" to a residual risk the operator must consciously accept before enabling unattended merge.**

---

## 10. Liveness & stall detection

> **The premise this refutes:** "the heartbeat arrived, therefore the job is fine." A heartbeat proves the **worker loop** is alive and reachable. It says **nothing** about whether the **agent** is *converging*. A worker can faithfully POST `/heartbeat` every 30 s for an hour while the agent it supervises is wedged in a tool-call retry storm, spinning on a spec contradiction, or burning $40 of tokens producing zero net diff. Worker-alive ≠ agent-progressing.

### 10.1 Why a single signal cannot be trusted

Two independent failures make a naive check worthless: (1) **the cheap signals are worker-reported, and the worker is untrusted** (T3) — a stuck or adversarial agent emits tokens/tool-calls as readily as a healthy one, *deliberately* to look busy; trusting them is the self-report we refuse for sign-offs (I-9); (2) **the un-gameable signals are slow** — Flowbee's own wall clock only knows "it's been 25 minutes," and the external oracle only updates on the reconcile-IN sweep (2–5 min) and abstains when there's nothing to observe. So the detector is a **ladder ordered cheapest/most-gameable → slowest/un-gameable**, with kill authority concentrated at the un-gameable end. Cheap rungs *suspect*; only expensive rungs *condemn*, and even they need a corroborating second opinion.

### 10.2 The five rungs

```
 cheapest, most gameable ───────────────────────────────────────▶ slowest, un-gameable (kill authority)
 ┌────────┬───────────────────────────────────┬───────────────┬─────────────┬──────────────┐
 │ Rung0  │ Rung1                              │ Rung2         │ Rung3       │ Rung4        │
 │ worker │ corroboration-gated progress       │ external      │ Flowbee     │ escalation   │
 │ super- │ accounting (tokens/tool_calls/     │ progress      │ clock-only  │ governor     │
 │ visor  │ edits/checkpoints + COST)          │ oracle        │ deadlines   │ (anti-thrash)│
 │ (HINT) │ frozen-vs-spinning; never kills    │ (reconcile);  │ (full kill) │              │
 │ 0 kill │ alone                              │ ABSTAINS blind│             │              │
 │ auth*  │                                    │               │             │              │
 └────────┴───────────────────────────────────┴───────────────┴─────────────┴──────────────┘
   *except locally-provable agent_exited_zombie (a free fast-path, §10.6)
```

**Rung 0 — worker-local agent supervisor (HINT only).** Reports `agent_health ∈ {ok, zombie, stdin_block, cpu_spin, oom, hung_child}` — the richest signal and the **least trustworthy** (it lives inside the untrusted box). **Zero kill authority**, with one exception: `agent_exited_zombie` — the supervisor `waitpid`s and sees the agent PID actually exited (*locally provable*) → a free fast-path to `failed` (§10.6). Every other value is a hint that raises suspicion on Rung 1 and may shorten Rung 3's soft deadline — never a kill alone.

**Rung 1 — corroboration-gated progress accounting.** The worker reports a *progress vector* — cumulative `tokens`, `tool_calls`, `edits`, `checkpoints`, plus **cost** (feeding I-15). Classification: *working* (vector + cost advancing), *frozen* (both frozen — likely hung/blocked), *spinning* (vector advancing, cost climbing fast, **no external corroboration** — busywork). **Corroboration-gated:** "spinning" means "Rung 1 says lots of activity, Rung 2 sees no matching convergence." Rung 1 **never kills alone** (a clever stuck agent makes its vector advance forever). It (a) feeds the cost ceiling, (b) raises a *suspicion flag* that, paired with Rung 2 or Rung 3, satisfies the two-rung rule, (c) distinguishes *frozen* from *spinning* so Rung 3's deadlines can be tuned.

**Rung 2 — externally-anchored progress oracle (on the reconcile sweep).** The first **un-gameable** rung, with partial kill standing. It looks only at evidence Flowbee reconciled *itself*: **net non-reverting, content-bearing diff convergence** of `refs/flowbee/<job>/epoch-<n>` against a sliding window (a branch that hasn't gained a net line of *meaningful* diff in 20 min while Rung 1 claims thousands of tokens is the canonical *spinning* signature, externally confirmed); and **CI-transition tolerance extension** (a GitHub-recorded CI transition is hard proof the agent's output reached the outside world; it **extends Rung 2's tolerance window** so a 40-min E2E suite isn't counted as "no new diff", §10.4). Rung 2 **ABSTAINS when blind** — it returns `abstain`, never `stalled`, whenever it has nothing to observe (spec-flow jobs with no SHA, a build job before its first ref push, or a degraded sweep). An abstaining Rung 2 contributes **no** vote. A **fleet-wide circuit breaker** guards against a wholesale reconcile outage making Rung 2 abstain everywhere: if Rung 2 abstains for too many jobs at once, Flowbee stops trusting clock-plus-Rung2 combinations, widens deadlines, and alarms rather than letting Rung 3 kill into a blind spot.

> **Spec-flow note:** all `spec_authoring`/`spec_review` states force **Rung-2 = abstain** (no SHA exists, §11), so spec-flow stall detection leans entirely on Rung-3 clock deadlines + Rung-4 governor.

**Rung 3 — Flowbee-clock-only deadlines (full kill authority).** Pure wall-clock arithmetic on **Flowbee's clock only**. The **un-gameable backstop** with **full kill authority**: a **per-phase soft deadline** (role/constraint-derived; crossing it arms the warn→cancel ladder but does not kill outright) and an **absolute lease cap** (`lease_deadline` — a stuck-but-heartbeating worker can **never** hold a job past it, the one guarantee that holds even if every other rung is gamed, partitioned, or abstaining). **Flowbee is the sole clock.** Rung 3 can condemn on its own clock, but per the kill rule a *soft-deadline* condemnation still needs a second rung; only the **absolute** cap revokes unilaterally.

**Rung 4 — escalation governor (anti-thrash).** Not a detector — the governor. It owns `stall_revocations` and the anti-thrash policy: each stall-driven revocation increments the counter; a bounded number (distinct from `max_attempts`/`max_bounces`) routes the job to **`needs_human`** rather than re-dispatching forever into the same stall; it rate-limits revocations fleet-wide and cooperates with Rung 2's circuit breaker. Rung 4 is *why* killing is safe to automate.

### 10.3 The two-rung kill rule (I-13)

> **A kill — revoking the lease of a presumed-stalled agent — requires TWO independent rungs in agreement, and at least one must be Rung 2 (external) or Rung 3 (clock). Never two worker-reported rungs.**

Requiring at least one un-gameable rung in every kill means **no amount of worker-side lying can either trigger a wrongful kill or prevent a rightful one.**

| Rung A | Rung B | Kill? | Why |
|---|---|---|---|
| Rung 1 `frozen` | Rung 0 `stdin_block` | **NO** | Both worker-reported. Suspicion only. |
| Rung 1 `spinning` | Rung 2 `no net diff` | **YES** | Worker says "busy," external oracle confirms "no convergence." |
| Rung 0 `cpu_spin` | Rung 3 soft-deadline crossed | **YES** | Clock corroborates the local hint. |
| Rung 3 soft-deadline | Rung 2 `abstain` | **NO** | Abstain is not a vote. Wait for a real second rung or the absolute cap. |
| Rung 3 **absolute cap** | — | **YES (unilateral)** | The hard ceiling; no interpretation of "progressing" survives. |
| Rung 1 `frozen` | Rung 3 soft-deadline | **YES** | Clock is the independent corroborator for the freeze claim. |

The only unilateral kill is the **absolute lease cap** (Rung 3). The only other unilateral terminal transition is the **`agent_exited_zombie` fast-path** (§10.6) — and that is a `failed` transition, not a "kill" (the agent is already dead and the worker proved it locally). A "kill" *is* a **lease revocation** — Flowbee bumps `lease_epoch` (so the zombie's next fenced call is rejected `409`, I-4) and fires explicit compensation (§6.5). It is **not** Flowbee reaching into the worker to SIGKILL anything; Flowbee cannot, and must not need to (R2).

### 10.4 False-positive guardrails

The worst failure is killing healthy work. Two legitimate patterns look like stalls:

**Guardrail A — the 40-minute E2E.** Per-phase deadlines are **role/constraint-derived, not global** (a job carrying derived `os:ubuntu-24.04`+playwright E2E constraints inherits the *E2E phase budget*); and **a CI transition extends Rung 2's tolerance** — the moment GitHub records the suite as `running`, "no new diff" is *expected*, not stalled.

**Guardrail B — the long reasoning step.** A model can legitimately spend many minutes in a single reasoning turn (Rung 1 *frozen* but the agent is *thinking*). **Frozen ≠ dead:** frozen alone *cannot* kill (the two-rung rule needs Rung 2 or Rung 3 to agree, and during a legitimate reasoning step neither does). The soft deadline absorbs it; only the *absolute* cap is hard. `agent_health: ok` + frozen reads as "thinking," distinct from `stdin_block`/`zombie` + frozen ("hung").

The design bias is explicit: **a false negative (a stalled job that survives a little too long) costs minutes and dollars; a false positive (a healthy job killed mid-E2E or mid-reasoning) costs the whole attempt and re-queues poison.** The two-rung rule, abstain semantics, and CI-tolerance extension all bias toward the cheaper error.

### 10.5 Partition vs. stall (LAN / Tailscale)

A worker on a separate box (R5) can lose the network while its agent keeps running. From Flowbee's side the *symptom* — heartbeats stop — is identical to a hung agent. Conflating them is a correctness bug. Flowbee distinguishes them:

- **Heartbeat silence is classified `worker_unreachable`, not `agent_stalled`.** It feeds Rung 3's lease clock (the absolute cap still applies — an unreachable worker can't hold a job forever) but does **not** by itself satisfy the agent-stall kill rule. Rung 2 keeps seeing the *last* pushed diff unchanged, consistent with both "partitioned" and "stalled," so it **abstains**.
- **The reconnecting zombie is handled by fencing, not by a kill** (I-4, T2). If continued silence past a threshold causes reassignment (epoch bump → re-lease to another worker), the original worker's healed-link `result`/`heartbeat` carries a now-stale epoch and is **rejected `409`**; its epoch-namespaced ref is **never fast-forwarded** (I-7, I-12). We get exactly-once *acknowledgement* even though the partitioned worker may have *executed* a full build (T2).
- **Grace before reassignment** is generous (a multiple of the heartbeat interval, bounded by the absolute lease cap): a 30-second Tailscale hiccup must not reassign; a worker gone for the entire cap must.

Net: a partition costs *latency*, never *correctness*.

### 10.6 The two free fast-paths

Two transitions are unambiguous enough to bypass the ladder — "free" because the evidence is conclusive on its face:

1. **`awaiting_input → cancel`.** The agent is blocked on human/interactive input that will never come → no progress is possible. Flowbee issues `directive: cancel`, releases the lease cleanly, routes per policy (typically `needs_human`). No deadline wait.
2. **`agent_exited_zombie → failed`.** The Rung-0 *locally provable* exit: the supervisor `waitpid`s and sees the agent PID died. The one Rung-0 signal with standing, precisely because the worker proves it on its own machine. Straight to `failed`; compensation fires; the job re-queues (subject to `max_attempts`).

### 10.7 State-machine deltas

Liveness threads into the §6 state machine as **sub-state on the lease** plus a governor counter, driven by **the in-process timer loop** (the `timers` table + polling goroutine, §12.3) — not a new top-level state.

| Field | On | Meaning |
|---|---|---|
| `lease_epoch` | lease | (I-4) bumped on every revocation; fences the zombie |
| `last_heartbeat_at` | lease | drives `worker_unreachable` classification |
| `progress_vector` | lease | last Rung-1 vector (tokens/tool_calls/edits/checkpoints/cost) |
| `agent_health` | lease | last Rung-0 enum (hint) |
| `phase_deadline_at` | lease | Rung-3 per-phase **soft** deadline (role/constraint-derived) |
| `lease_deadline` | lease | Rung-3 **absolute** cap |
| `rung2_last_verdict` | job | `{converging \| stalled \| abstain}` from the last sweep (canonical enum) |
| `stall_revocations` | job | Rung-4 governor counter — **distinct** from `attempts`/`bounces` |

```
   leased/active ──soft deadline + 2nd rung──▶ WARN ──grace──▶ CANCEL ──no clean release──▶ REVOKE
        ▲                                        │                                            │ epoch++
        └────────── clean release / result ◀── recover & continue                             │ compensation
                                                                                               ▼
                                                                                  job → ready (re-dispatch)
```

| Step | Trigger | Action | §6 effect |
|---|---|---|---|
| **WARN** | soft deadline crossed **and** a corroborating rung (two-rung rule armed) | log + alarm; shorten next heartbeat window; ask Rung 2 for a fresh verdict | job stays active |
| **CANCEL** | two-rung rule satisfied, agent reachable | `directive: cancel`; await clean `release` | job stays active, pending release |
| **REVOKE** | cancel not honored, or unreachable past grace, or absolute cap hit | `lease_epoch++`; **compensation** (close zombie draft PR, drop epoch ref — I-12); release lease | active → **`ready`**, `attempts++` |
| **BOUNCE** | re-dispatch on revoke | re-lease to a *different* eligible worker (anti-affinity still holds) | `ready → leased` (new epoch) |
| **needs_human** | `stall_revocations` ceiling (Rung 4), **or** `max_attempts`/`max_bounces` exhausted (I-6) | stop re-dispatching; escalate | active → **`needs_human`** (non-terminal) |

**The `timers` table + one polling goroutine** drive every wait (`phase_deadline_at`, `lease_deadline`, the WARN→CANCEL grace, the partition-grace are all rows in the `timers` table; the reconcile sweep producing `rung2_last_verdict` is a periodic timer-driven job). The lease primitive remains **custom on top of the SQLite store** (§12.3); liveness adds *timers and counters around* it. Each timer carries `expected_epoch` and no-ops if the epoch moved, so a stale deadline check is idempotent. Every revocation is transactional with the epoch bump and compensation enqueue, so a crash mid-revoke replays cleanly.

### 10.8 MVP cut

Per T1's scope discipline, liveness ships **lean** so the *un-gameable* protections exist day one and the *gameable, tuning-heavy* ones are deferred:

**In the MVP:** Rung 3 (clock deadlines — the un-gameable floor); Rung 4 (governor + `→ needs_human` — without it automated killing isn't safe); a **minimal Rung 2 gate** (net-diff-convergence-or-abstain on the existing sweep + CI-transition tolerance + the fleet-wide circuit breaker — the abstain-heavy version that provides the *one external corroborator* the two-rung rule needs); the two free fast-paths; the two-rung kill rule itself (I-13) over {minimal Rung 2, Rung 3}; and partition-vs-stall classification + fencing-handles-the-zombie (falls out of the lease primitive).

**Deferred:** full Rung 0 (the rich `agent_health` enum beyond the zombie fast-path — gameable, hint-only); full Rung 1 (the complete frozen-vs-spinning analytics — useful for *tuning*, not *correctness*); adaptive priors (learning per-role/per-repo durations and convergence windows to auto-size deadlines — the MVP uses static, constraint-derived budgets).

The cut honors §10.4's bias: even at MVP, the system is **more likely to let a stalled job run a few minutes long than to kill a healthy E2E or a long reasoning step**.

---

## 11. The spec-review gate (the pre-SHA flow)

This is the stage the draft structurally **cannot** model. The draft's state machine began at "build an issue," and every verdict bound to a `(head_sha, base_sha)` pair. But Sam's pipeline begins one gate earlier — at a **chat that produces a spec** — and that gate fires **before any branch, SHA, or PR exists**. There is nothing for a verdict to bind to. The spec-review verdict is **pure Domain-A state** (§3.1): no GitHub object holds "a spec was signed off, under lens L, by an identity distinct from its author." So unlike the code-review gate, this gate's sign-off is **not** reconciled against a GitHub-owned fact — there is none. It is reconciled against the only ground truth that exists pre-SHA: the bytes of the spec itself, addressed by content-hash.

### 11.1 The artifact under review: a committed `spec.md`, not the bare issue body

**Decision: the spec is a `spec.md` file committed to a dedicated Flowbee-owned spec branch `refs/flowbee/spec/<job>` (§3.4, §6.1), not the issue body.** The verdict binds to that file's `spec_content_hash`.

| Property the gate needs | Bare issue body | Committed `spec.md` (chosen) |
|---|---|---|
| **Content address** to bind a verdict to | none — mutable text with only a coarse `updated_at` | a real tree-hash → `spec_content_hash`, exactly as the build verdict binds to `head_sha` |
| **Versions** across revisions | unaddressable edit history | every revision is a commit on the spec branch — an ordered, immutable chain `v1→v2→v3` |
| **Diffs** for re-review | none | `git diff v(n-1)..v(n)` — the reviewer judges the **delta**, the cheap correct re-review |
| **Tamper-evidence** | an edit silently changes the approved thing | a content-hash move **supersedes** the sign-off mechanically (§11.5), like a SHA move |
| **Lineage anchor** | issue number only | the spec commit is the canonical root of `chat→spec→issue→build→PR→review→merge` |
| **Domain placement** | tempts treating the issue as the source (the §3.2 inversion error) | the issue is a **rendering**; the spec branch is the Flowbee-owned source |

The issue body is **not** discarded — it is *rendered out* (project-OUT) from `spec.md` once the gate passes, so humans browsing GitHub see a readable issue. But it is **output**, never the artifact under review. Editing it on GitHub is graffiti on a rendering; it does not re-open the spec gate, and project-OUT reasserts the rendered body (except on ADOPT-quiesced objects, I-16).

```
   refs/flowbee/spec/<job>     (Flowbee-owned spec branch — the SOURCE, §3.4)
   ┌──────────┬──────────┬──────────┐
   │ spec v1  │ spec v2  │ spec v3  │      each commit = an addressable revision
   │ commit a │ commit b │ commit c │      spec_content_hash = BLAKE3 tree-hash of spec.md @ HEAD
   └────┬─────┴────┬─────┴────┬─────┘
        │rejected  │rejected  │SIGNED-OFF (verdict binds to hash(c))
        ▼          ▼          ▼
     revise     revise    materialize issue(s) ──project-OUT──▶ GitHub issue (a RENDERING)
```

The spec branch is **Flowbee-owned**, like the epoch-namespaced build refs (§3.4, I-7). Workers do not push to it. The `spec_author` returns spec prose through the work-product channel (`emits: spec_doc`); **Flowbee** commits it to the spec branch and computes the hash. This keeps the author from forging its own "approved" content-hash, mirroring the rule that the `eng_worker` never opens its own PR (§3.5).

### 11.2 The `spec_review` job

A `spec_review` job is the `review` stage of the spec flow (`job.kind = spec`, stage `review`, role `spec_reviewer`). **Input** is delivered via the unified lease envelope of §7.2 (`job.job_id`, top-level lease fields) with the spec-only `job.spec` block. What is **deliberately absent** distinguishes it from every job the draft modeled: no `base_sha`, no `head_sha`, no PR. The content address is `spec_content_hash`.

**Context discipline (I-10 in the spec flow).** The `spec_reviewer` receives the spec text and the diff (`context: spec_and_diff_only`, §5.3). It does **not** receive the raw chat transcript *as authority*. The chat is lineage, not evidence (§11.6) — the spec-flow analogue of "judge the diff, not the attacker's narration": *judge the spec, not the author's chat-side salesmanship.*

### 11.3 The verdict schema

The reviewer answers **two** orthogonal questions plus a disposition, mapping to the two things Sam said the spec reviewer checks: **engineering style** and **project requirements**.

```jsonc
// POST /v1/jobs/j_spec_8841/result  — the reviewer's CLAIM (not yet the sign-off)
{
  "lease_id": "l_113", "lease_epoch": 7, "idempotency_key": "…",
  "work_product": {
    "kind": "verdict",
    "binds_to": "blake3:9c2f…",          // the spec_content_hash judged — MUST match the lease
    "spec_version": 2,
    "meets_engineering_style": {         // Q1 — eng standards expressible at spec time
      "result": "pass",                  // pass | fail
      "findings": [ /* structured, each tied to a spec-section anchor */ ]
    },
    "meets_requirements": {              // Q2 — satisfies project requirements / is buildable
      "result": "fail",
      "findings": [ { "anchor": "#testability", "severity": "blocking",
                      "note": "no acceptance criteria for the merge-queue path" } ]
    },
    "decision": "changes_requested",     // signed_off | changes_requested
    "summary": "<reviewer prose — recorded as a claim, shown to author, NOT authority>"
  },
  "status": "succeeded"                  // a HINT only — NOT the sign-off (I-9)
}
```

**The sign-off is `decision == signed_off` AND both sub-checks `pass`.** Gate logic (not the worker) enforces that conjunction when it mints the record (§11.5 step 3). A `meets_requirements` finding at `severity: blocking` forces `changes_requested` regardless of the worker's `decision` — the `decision` field is a *claim*; Flowbee concludes the verdict (I-9).

### 11.4 How sign-off gates the build flow

The spec gate is a **hard dependency edge** in the job DAG, expressed with the same `blocked_by` primitive — pointing at a `spec_review` job rather than an issue.

```
 spec flow:   [spec_author] ──▶ [spec_review]══════════╗  (GATE)
                                                        ║ sign-off record (Domain-A, content-hash-bound)
 build flow:                     [eng_worker] ◀═════════╝
                                  blocked_by: { spec_review: j_spec_8841,
                                                requires_signoff_for: blake3:9c2f… }
```

A `build` job is `ready` (leasable) **only when** its `blocked_by` spec_review job holds a sign-off whose `binds_to` equals the **current** `spec_content_hash` of the spec branch. The `eng_worker` lease then carries `base_sha` (the build flow gains a SHA here) **and** the authorized `spec_content_hash`, so the build's lineage records *which spec version* it implements. This is I-5 relocated: where the build flow says "CI-green AND review-valid bound to the exact `(head_sha, base_sha)`," the spec gate says "**build authorized AND spec signed-off bound to the exact `spec_content_hash`.**" There is exactly one legal way for `eng_worker` to start: a content-hash-bound sign-off minted by Flowbee gate logic (I-9).

### 11.5 What replaces SHA-pair binding: the spec content-hash

> A spec sign-off is a tamper-evident Flowbee record **bound to a `spec_content_hash`** (BLAKE3 tree-hash of `spec.md`), derived by gate logic from the reviewed spec bytes — never a worker-self-reported `status: succeeded`. **Any edit to the spec supersedes the sign-off and re-arms the gate**, exactly as a base/head SHA move supersedes a code-review verdict (I-5).

How Flowbee mints it (the spec-flow parallel to §5.5's reconcile-then-mint, adapted because the ground truth is *bytes*, not a GitHub fact):

1. **Record the verdict as a claim**, fenced by `lease_epoch`, attributed to the reviewer identity. The worker's `summary` prose is stored as a claim — shown to the author, never authority.
2. **Verify the binding:** the claim's `binds_to` must equal the spec branch's **current** `spec_content_hash`. If the spec advanced mid-review (a v2→v3 edit landed), `binds_to` is stale → the verdict is **rejected as superseded**, the gate stays armed, a fresh `spec_review` job is enqueued against v3. This is the base/head-drift supersession of I-5 with `spec_content_hash` in the role of the SHA pair.
3. **Apply the conjunction:** `signed_off` minted **iff** `decision==signed_off ∧ meets_engineering_style.pass ∧ meets_requirements.pass`; otherwise `changes_requested`.
4. **Mint the tamper-evident record** — `{spec_content_hash, spec_version, reviewer_identity, reviewer_lens, decision, ts, claim_digest}` — into Domain-A lineage. On `signed_off`, this clears the build gate (§11.4) and triggers `materialize_issues` (project-OUT renders the GitHub issue from `spec.md`).

Because the binding is to content, supersession is **mechanical and total**: the instant `spec.md` changes (a new commit, new hash), every sign-off bound to the prior hash is dead. A build that hasn't started cannot start; a build already in flight against the old spec is superseded and re-armed (the `superseded` machinery, §6.2.4). No human can "approve a spec" and then quietly edit it into something else: the edit revokes the approval by construction.

```
   spec_content_hash = H1 ──▶ sign-off(H1) ──▶ build gate OPEN for H1
        │  human or spec_author edits spec.md
        ▼
   spec_content_hash = H2 (≠ H1) ──▶ sign-off(H1) SUPERSEDED ──▶ build gate CLOSED
        ──▶ fresh spec_review job enqueued against H2
```

### 11.6 The chat→spec→issue entry: the first Flowbee job, and where chat lives

**Decision: the chat is OUT of the job graph; it is a lineage root, not a job.** A chat has no lease, no deterministic completion, no verdict, no content-hash — modeling it as a job would be a category error. Flowbee does not orchestrate the chat; it **ingests the chat's product** and records the chat as a **lineage node** (`chat_ref`) anchoring the root of `chat→spec→issue→…`. The chat is referenced by every descendant for provenance but is never `ready`, `leased`, or `done`.

**The first Flowbee job is `spec_author`'s materialization of the spec**, created when the chat hands off a spec draft:

```
  Sam ──chat──▶ spec_author agent          (OUTSIDE the job graph; chat_ref = lineage root)
                      │ agent emits spec_doc via work-product channel
                      ▼
   Flowbee CREATES the first job:  ┌────────────────────────────────────────┐
                                   │ job j_spec_8841  kind=spec  stage=author │
                                   │ • commit spec.md → refs/flowbee/spec/…   │  ← Flowbee commits,
                                   │ • compute spec_content_hash (BLAKE3, v1) │     not the worker
                                   │ • lineage.chat_ref = c_5521 (root)       │
                                   └────────────────────────────────────────┘
                                                  ▼ enqueue spec_review stage (§11.2)
                                          [spec_review GATE]
```

Two wirings, both on the same primitive: **agent-initiated** (the `spec_author` worker POSTs the spec draft via the work-product channel; Flowbee commits, computes the v1 hash, creates the job, enqueues the review) and **human-initiated** (Sam or a thin chat surface POSTs a spec doc directly). Either way, **Flowbee commits the spec and owns the hash** — the author never self-addresses its artifact. The job graph's true root is the `spec_author` materialization job; the chat is its lineage parent, just outside the graph.

### 11.7 The reject→revise loop

A `changes_requested` spec verdict bounces to the `spec_author` stage — and the "author" on the revise arm may be **either the human in chat or the agent**, because the loop targets the *stage*, not a fixed actor.

```
 [spec_author] ──spec v_n──▶ [spec_review] ──changes_requested──┐
       ▲                                                        │ verdict (Domain-A claim, bound to H_n)
       │  revise → produce spec v_(n+1)                         ▼
       └────  agent: re-leased with findings + git diff   bounce (new commit, v_(n+1), new hash H_(n+1))
              human: Sam edits in chat, re-submits               ▼
                                                          [spec_review] re-review (lease carries
                                                          spec_diff = H_n..H_(n+1) + prior_verdicts)
```

Loop mechanics, reusing established primitives: the bounce is the existing `changes_requested → bounce` transition (`on_changes_requested: { bounce_to: author, max_bounces: 3 }`, §5.3). **Each revision is a new commit** on `refs/flowbee/spec/<job>`, raising `spec_version` and producing a new `spec_content_hash` — the immutable, ordered commit chain *is* the version history. **Re-review judges the delta** (the lease carries `spec_diff` and the reviewer's `prior_verdicts`, §7.2). **Anti-affinity holds:** the spec flow's term is `spec_author.lens != spec_reviewer.lens` (§5.5, I-10), enforced at every re-review lease. **`max_bounces` → `needs_human`** (I-6). **Cost ceiling applies** (I-15): each revise round meters tokens/$ against the per-flow ceiling.

### 11.8 Worked example (end to end)

```
 t0  Sam chats with spec_author agent.  chat_ref=c_5521  (lineage root, NOT a job)
 t1  Agent hands off draft. Flowbee commits spec.md→refs/flowbee/spec/j_8841 @v1, hash=H1.
     Creates job j_8841 (kind=spec). Enqueues spec_review stage.
 t2  Worker w_A (lens=staff_engineer, family=opus) leases spec_review. base_sha? none. Judges H1.
 t3  Verdict CLAIM: meets_engineering_style=pass, meets_requirements=FAIL (#testability blocking),
     decision=changes_requested, binds_to=H1.
     Gate logic: binds_to==current H1 ✓; conjunction fails → record=changes_requested.
 t4  Bounce → spec_author. Agent (or Sam) revises. Flowbee commits @v2, hash=H2. Gate armed for H2.
 t5  Re-review lease carries spec_diff=H1..H2 + prior_verdicts. Worker w_A (lens still ≠ author lens)
     judges. Both sub-checks pass, decision=signed_off, binds_to=H2.
 t6  Gate logic: binds_to==current H2 ✓; conjunction holds → MINT sign-off(H2).
     → materialize_issues: project-OUT renders GitHub issue #1402 from spec.md@H2.
     → build gate OPENS for H2.
 t7  eng_worker build job becomes ready; lease carries base_sha + authorized spec_content_hash=H2.
     Build flow (§5–§6) begins. SHA now exists.
 ── counterfactual ──
 t6' Before the build leases, Sam edits spec.md → @v3, hash=H3.
     sign-off(H2) SUPERSEDED. Build gate CLOSES. Fresh spec_review enqueued @H3.
```

### 11.9 Where T1 lands on this gate

The spec gate is **upstream of the human-merge question** and largely orthogonal to it — it gates *whether a build is authorized to begin*, not *whether a result may merge unattended*. So most of this section is MVP-load-bearing under **either** branch. The one coupling: the content-hash supersession (§11.5) and the committed-`spec.md` artifact (§11.1) are the spec-flow expression of the *same* tamper-evidence discipline I-9/I-11 bring to the build flow. If T1 keeps the human merge gate, a thinner v0 is defensible (gate on the bare issue body with a coarse `updated_at` watermark, accepting the loss of addressable diffs and mechanical supersession). If T1 takes the gate off, the committed-`spec.md` + content-hash binding is **not** optional polish — it is the only thing that makes "a spec was approved" a tamper-evident fact rather than an editable suggestion.

---

## 12. Deployment topology, storage & operations

Where Flowbee runs, what it persists, how it survives a crash, and how an operator sees inside it. Downstream of two non-negotiables: workers **dial out** (R2), so Flowbee needs zero inbound knowledge of where they are; and Flowbee is the **process system-of-record** (R3), so a boot reconstructs from `the Flowbee SQLite store + the reconciled GitHub-owned facts` and nothing else.

### 12.1 Topology is set by one fact: workers dial OUT

Because every worker is an outbound long-poll client, Flowbee never holds an address, port, or route for any worker — only a credential allowlist (§9) and a lease table. Same-box and cross-box are **identical at the protocol layer**; they differ only in *repo provisioning* (R5) and *network substrate*.

```
   ┌────────────────── ONE Flowbee process (single Go binary) ──────────────────┐
   │  PUBLIC :8443  webhook endpoint (HMAC-only, I-2)   ── internet-facing       │
   │  PRIVATE :7000 worker API (mutual-auth)  reconcile/project loops  SQLite     │
   └───────▲──────────────────────────────────────────────────────▲────────────┘
           │ long-poll GET /v1/lease (dial OUT, PRIVATE listener)  │
   ┌───────┴────────┐                   ┌───────┴────────┐  ┌───────┴─────────┐
   │ worker (same   │                   │ worker (LAN)   │  │ worker          │
   │ box, loopback) │                   │ 10.0.0.x       │  │ (Tailscale)     │
   │ codex/build    │                   │ opus/review    │  │ merger          │
   └────────────────┘                   └────────────────┘  └─────────────────┘
        worktree off                       bundle OR              bundle OR
        shared local mirror                scoped-read (R5)       scoped-read (R5)
```

**Two distinct listeners, sharing no trust path** (the §9 webhook/worker-API separation made concrete): a **public, HMAC-only webhook endpoint** (the *only* port that must face the internet; it does no business logic — verify HMAC, dedupe, write-ahead inbox, enqueue refetch) and a **mutual-auth worker API** bound to loopback/Tailscale (every call mTLS/token-authenticated against the enrolled-identity allowlist). A forged webhook can at most trigger a refetch that reads real state (§8.1.3); it has no path to the worker API or to leasing job context.

| | **Single-box** | **Multi-box (LAN / Tailscale)** |
|---|---|---|
| Flowbee↔worker transport | HTTP/JSON over **loopback** (`127.0.0.1:7000`) | HTTP/JSON over LAN IP or Tailscale MagicDNS name |
| Inbound to Flowbee from workers | none — workers dial out | none — workers dial out |
| Inbound from GitHub | webhook endpoint (HMAC, I-2) — the only internet-facing port | same |
| Repo provisioning (R5) | `git worktree` off a **shared local mirror** | **bundle** or a **scoped READ credential** |
| Worker auth (§9) | per-worker token over loopback | per-worker token over Tailscale WireGuard or mTLS on raw LAN |

Operator mental model: **scale by pointing more boxes at one URL.** A new arm64 Studio joins by running the worker binary with `FLOWBEE_URL=https://flowbee.taild0g.ts.net:7000` and an enrolled token — no Flowbee-side config, no inbound firewall rule. Capability matching (§5.6) and the arch-aware derived constraints route the work; topology is invisible above the transport.

### 12.2 Transport stays HTTP/JSON long-poll — explicitly NO gRPC/bus

The requirements do not justify the operational tax of gRPC streams or a message bus for v1.

| Candidate | Why it is *not* needed | What it would cost |
|---|---|---|
| **gRPC / bidi streaming** | the only server→worker signal is `directive: continue\|cancel` returned on the worker's *own* heartbeat; no server-initiated push is required | proto toolchain, HTTP/2 keepalive tuning, harder curl-debuggability across Tailscale |
| **Message bus (NATS/Redis Streams)** | the job *state engine* is the SQLite store + in-process dispatch loop (§12.3); a bus is a stream, not a job-state engine; leases need a fenced, transactional atomic claim a bus can't give | a new server type to run/HA, a second source of truth, no transactional coupling to the DAG |
| **WebSocket** | same as gRPC — no server-initiated push to justify a persistent socket | connection-state bookkeeping for a non-need |

`GET /v1/lease` long-polls ~30 s and returns `204` if nothing is eligible (a held request *is* the "push"); `POST …/heartbeat` carries `lease_epoch` and returns `{continue|cancel}` (the cancel directive signals supersession/revocation **without any inbound channel to the worker**). **SSE later:** a read-only `/v1/events` stream is a *post-MVP* affordance for the dashboard and lower-latency `cancel`, layered on without disturbing the long-poll core — never the lease-delivery path. A *partitioned* worker simply cannot reach `/heartbeat`; Flowbee's clock expires the lease and fencing (I-4) rejects the reconnecting zombie — the transport choice makes a partition *look like* a partition (clean lease expiry) rather than a stall.

### 12.3 Store: embedded SQLite underneath, the custom agent-lease primitive ON TOP

> **Reconciled to the built engine.** The pre-build draft named **River/Postgres** here. The shipped engine uses **embedded SQLite as the single store of record** and a **hand-rolled in-process timer/dispatch loop** in place of River. This section is the canonical statement; the substrate is SQLite, not Postgres, and there is **no River dependency**.

Flowbee runs **embedded SQLite** (`modernc.org/sqlite` — a pure-Go SQLite, no cgo, so the binary stays `CGO_ENABLED=0` and statically linked), in-process, as the durable store of record, and layers a **custom agent-lease primitive** on top. Two distinct layers; conflating them is a modeling error — **agent leases are NOT plain queued rows.**

```
   ┌─────────────────────────────────────────────────────────────────────────┐
   │  FLOWBEE FLOW ENGINE  (deterministic core, I-0 — pure Decide())           │
   ├─────────────────────────────────────────────────────────────────────────┤
   │  CUSTOM AGENT-LEASE PRIMITIVE  (renewable TTL + heartbeat + fencing)       │
   │    • atomic claim (§6.3.1) + partial unique index "one active lease/job"   │
   │    • lease_epoch fence: stale-epoch mutations → 409   (I-4)                │
   │    • Flowbee-clock TTL + lease_deadline cap (Rung-3 floor, I-13)           │
   ├─────────────────────────────────────────────────────────────────────────┤
   │  EMBEDDED SQLite  +  in-process timer/dispatch loop                        │
   │    • single connection (SetMaxOpenConns(1)) — serialized writes, no MVCC   │
   │    • '?' placeholders, TEXT/RFC3339 + datetime('now') (no TIMESTAMPTZ)     │
   │    • INTEGER PRIMARY KEY AUTOINCREMENT; partial unique indexes;            │
   │      UPDATE … RETURNING for the atomic claim                               │
   │    • `timers` table (due_at, expected_epoch, fired) + ONE polling          │
   │      goroutine = cadence/deadlines/alarms; transactional enqueue,          │
   │      epoch-guarded idempotent deadline checks, dedup, sweeper              │
   └─────────────────────────────────────────────────────────────────────────┘
                         all in one process, one SQLite file
```

**The SQLite store + timer loop buys** (the boring-but-bug-prone substrate we refuse to hand-roll into the lease): transactional **enqueue** coupled to the same SQLite transaction that mutates job state (the outbox row and the Domain-A state change commit together); **retries with backoff** for *internal* Flowbee work (a project-OUT push that 403s parks on `Retry-After`; a sweep that times out re-arms); **timers/scheduled jobs** (the reconcile cadence, the `no_eligible_worker` alarm, per-phase soft deadlines, `max_bounces` escalation) via the `timers` table and the single polling goroutine; **dedup** (a replayed webhook doesn't fan out duplicate reconcile work — the `(job, action, head_sha)` outbox key and inbox `X-GitHub-Delivery` dedupe); a **sweeper** for timers orphaned by a crash (reconstruct-on-boot re-arms from the table). **Why SQLite, not Postgres:** the system is single-operator, single-repo, single-writer; `SetMaxOpenConns(1)` makes writes serialized (no MVCC needed for the partial unique index to hold), and the single-file store keeps the "single static binary, zero external services" property the deployment model (§12.1) is built on. Postgres/River would add an external server to run and an HA story Flowbee does not need at this scale.

**What the lease must NOT delegate, and why it is custom:** a generic queue gives *a worker grabs a row, works it, acks it* — at-least-once execution of an **internal** unit of work. An **agent lease is a different object**: a *renewable, heart-beated, time-boxed, fenced claim held by an untrusted external process across the network*, whose liveness is judged by **Flowbee's clock**, and whose revocation must drive **explicit compensation** (close the zombie draft PR, drop the epoch ref — §6.5) rather than a silent retry. The store has no native concept of `lease_epoch` fencing, no `directive: cancel` mid-flight, no `k·heartbeat` TTL renewal, no two-rung kill rule (I-13). Therefore the agent-facing lease lives in **Flowbee-owned tables** with the §6.3.1 atomic-claim (`UPDATE … WHERE state='ready' … RETURNING`) and the partial unique index. The timer loop *fires the deadline checks* (each carries `expected_epoch`; on run it re-reads the job and **no-ops** if the epoch moved — a stale timer is idempotent); it does not *own the lease*. One SQLite file, one Go process, single static binary; the `jobs`, `leases`, and `lineage` tables stay directly SQL-queryable (which makes §12.6's dashboard and single auditable log cheap).

### 12.4 SPOF / HA: one binary, reconstruct-on-boot, documented RPO

Flowbee is a **single Go binary** and, in v1, a **single process** — an honest SPOF. We make the failure *recoverable* and *bounded*, the correct v1 posture for a single-operator, single-repo system.

**Boot = reconstruct from two sources, in order:** (1) the **Flowbee SQLite store** is authoritative for **Domain A** (the entire process state-of-record — the source GitHub *cannot* reconstruct); (2) **reconcile-IN** re-pulls the **Domain-B facts** and corrects drift accumulated while down (missed webhooks caught by the delivery high-water-mark forcing a full sweep, I-1). The §3 asymmetry makes the boot safe: **you cannot lose the job DAG and rebuild it from GitHub**, so store durability is protected hardest:

| Mechanism | Purpose | RPO |
|---|---|---|
| **SQLite WAL mode + litestream-style continuous replication** (or periodic `.backup`) of the single DB file | survive disk/box loss of the primary | bounded by replication lag — **document it** (target: seconds) |
| **Idempotent, re-checkable external actions** | a crash mid-merge is safe: on boot, reconcile-IN asks GitHub "is PR #N already merged?" before any re-dispatch (I-3 terminal-SHA guard) | a settled merge is *never* re-driven |
| **Epoch-namespaced side effects** (§3.5, I-12) | a crash that orphaned a worker's push leaves a `refs/flowbee/<job>/epoch-<n>` ref never fast-forwarded without epoch validation | no torn promotion |
| **launchd `KeepAlive`** (macOS host) | restart the binary on crash/exit; with reconstruct-on-boot, an unattended crash self-heals | recovery-time, not data-loss |

> **SQLite is the store of record (not a caveat — the decision).** The shipped engine stores Domain A in a single embedded-SQLite file. Because losing the job graph to disk loss is unrecoverable (Domain A is not in GitHub), durability rests on **WAL mode + litestream-style continuous replication + a documented RPO**. This is the right floor for a single-operator, single-repo, single-writer system; Postgres is **not** required and is **not** used. (The pre-build draft inverted this — naming Postgres the floor and SQLite the caveat — before the single-writer scale was settled. Reconciled: SQLite is the store of record.)

**HA is explicitly v1.1+.** A true active/standby Flowbee (leader election, single-writer to the lease table) is deferred. The v1 bet: *reconstruct-on-boot + SQLite WAL replication + launchd KeepAlive + idempotent external actions* makes the SPOF a **bounded-downtime, zero-corruption** event rather than data-loss.

### 12.5 The flow engine is behind a clean interface (a contained later swap)

> **Reconciled:** the pre-build draft framed this as a "River → Temporal" swap. There is **no River**; the v1 substrate is SQLite + the in-process timer loop. The durable point stands: the flow engine sits **behind a clean interface**, so the store/dispatch substrate is a contained later swap.

The flow engine sits behind a clean interface; **SQLite + the in-process timer/dispatch loop** is the v1 implementation, not a permanent commitment. Moving to a heavier durable-workflow substrate (e.g. Temporal) is justified *only if* flows grow into long, branchy, multi-day, human-in-the-loop pipelines or the operator needs real HA — and even then it is contained to the flow-engine implementation, because the **lease primitive** and the **two-domain reconciliation** live *above* the flow engine and are engine-agnostic, the **work-product channel, epoch-namespaced refs, and sign-off rule** are protocol-level, and Domain-A persistence is in Flowbee-owned tables a substrate swap would not relocate. We **reject Temporal (and any clustered server) for v1**: its clustered server, determinism tax, and replay model kill the single-static-binary property, and its wins are *flow-engine* wins, not *dispatch* wins. The deterministic core (I-0) already gives us the replay property a workflow engine would sell us — without the dependency.

### 12.6 Observability: one board, one roster, gauges, one auditable log, cost meters

The operator surface is the SQL-queryable store rendered five ways. No separate telemetry pipeline in v1.

**1. The board — lanes are a documented projection of the canonical state set.** The lanes use canonical state names (§5.1 mapping); states are folded as noted so the board maps cleanly to §6.2:

```
 ┌ ready ┬ spec_authoring ┬ spec_review ┬ building ┬ code_review ┬ mergeable ┬ merging ┬ blocked ┬ needs_human ┐
 │ j_310 │ j_295          │ j_298       │ j_305    │ j_301       │ j_290     │ j_287   │ j_277   │ j_266       │
 └───────┴────────────────┴─────────────┴──────────┴─────────────┴───────────┴─────────┴─────────┴─────────────┘
   folded into their active lane: leased→(its active state)  review_pending→code_review
                                  merge_handoff→merging       superseded→ready(re-armed, badged)
   terminal (not lanes, shown in history): done, cancelled
```

`code_review` is shown under its **canonical** name (never "reviewing"). `needs_human` is non-terminal and is where I-6, I-11, and I-13 all deposit work — the operator's primary attention queue.

**2. The worker roster — who's connected, on what, where** (with topology so a partition is visible):

```
 worker_id  identity  model_family  caps(ATTESTED)                host                  lease    last_hb
 w_7f3      codex     codex         role:eng_worker arch:x86_64    mac-mini-1 (loopback)  l_9/e42  2s ago
 w_8a1      opus      opus          role:code_reviewer             studio (100.x ts)      —        4s ago
 w_2c4      codex     codex         role:eng_worker arch:arm64     iMac (10.0.0.5 LAN)    l_12/e51 91s ago  ⚠ stale-hb
```

A stale heartbeat (`w_2c4`) is the *worker-partitioned* signal — distinct from an agent stall (I-13, §10.5).

**3. Identity-budget gauges.** A live gauge of the **single installation token's** `rateLimit.remaining` (I-14 — *one* bucket to watch), plus project-OUT outbox depth and `Retry-After` backoff state. This is the instrument T1 calls for: **before deciding whether the human merge gate comes off, measure `rateLimit.remaining` for a week** (§13.4).

**4. One auditable action log.** A single append-only log of every GitHub-affecting action, keyed `(job_id, action, head_sha)`, each row attributed to the resolving identity and the reconciled fact that authorized it. Because Flowbee is the *only* GitHub caller (R4), this log is *complete* — the provenance trail behind every tamper-evident sign-off (I-9).

**5. Per-job / per-flow token-$ cost metering (I-15).** Every lease accumulates `{tokens_in, tokens_out, $}` reported on heartbeat/result, rolled up per job and per flow against an **enforced ceiling**:

```
 flow   job     role          tokens_in  tokens_out   $        ceiling   state
 build  j_305   eng_worker    412k       88k          $6.10    $10.00    ok
 build  j_301   code_reviewer 120k       14k          $1.90    $5.00     ok
 spec   j_298   spec_reviewer 44k        9k           $0.70    $3.00     ok
 build  j_277   eng_worker    980k       210k         $14.80   $10.00    ⚠ CEILING → escalate
```

Exceeding a ceiling **escalates** (to `needs_human`), it does not silently overspend (I-15). Per-flow rollup answers "what did this whole feature cost end-to-end across spec + build + review?"

### 12.7 Bootstrap: ADOPT mode for the first run against a live repo (I-16)

The first time Flowbee points at a repo with humans already working in it, the danger is that project-OUT sees a board full of issues/PRs it didn't create and starts **reasserting renderings over human-owned in-flight work** — relabeling, flipping draft↔ready, or scheduling a build on someone's half-finished PR. ADOPT mode makes the first run *inert toward existing work by default*.

```
   first boot against a live repo
            ▼
   ADOPT SWEEP ── reconcile-IN imports every open issue/PR as Domain-A jobs in
            │      state: mirrored-quiescent (full Domain-B facts reconciled; lineage roots created)
            ▼
   project-OUT SUPPRESSED on quiescent jobs ── no label/draft/check writes, no scheduling, no leases
            ▼
   OPT-IN gate ── a job leaves quiescent ONLY when opted in via:
            │        • a watermark (e.g. issues created after T0), OR
            │        • an explicit label (flowbee:adopt)
            ▼
   opted-in jobs enter the normal DAG (ready → …) ── project-OUT now renders them
```

Three rules make ADOPT safe: **(1) mirrored-but-quiescent is a real state** — adopted jobs are reconciled but **never scheduled**, so no worker ever touches human-owned in-flight work; **(2) project-OUT does not reassert renderings on adopted-quiescent objects** (the explicit §8.2.3 exception: a human's label on a quiescent job is *not* drift); **(3) opt-in is deliberate, by label or watermark** — the operator promotes work into Flowbee's control one decision at a time, and Flowbee **never seizes** work it didn't originate. The watermark (schedule only work created after `T0`) is the clean "start fresh" default; the `flowbee:adopt` label is the "pull this specific item in" escape hatch. ADOPT mode is the precondition for the strangler migration (§13) — it avoids a first-boot stampede over live PRs.

---

## 13. MVP & migration path

What we build first, and what we leave on the floor. The clean-room invariants (§15) force a sharp line between *the slice that kills the original pain* and *the slice that THE ONE DECISION (§14) gates*.

### 13.1 The strangler, not the rewrite

Flowbee replaces a working — if sprawling — pipeline. The migration is a **strangler fig**: Flowbee grows around the existing `samhotchkiss/russ` scripts, takes over one responsibility at a time, and each script is deleted only once its replacement reconciles correctly in production. No flag day.

| `russ` artifact (today) | Concern it owns | Strangled by | Cutover signal |
|---|---|---|---|
| `reconcile.go` | batched GraphQL sweep → `state.json` | **reconcile-IN** loop (Domain-B facts → store) | Flowbee mirror matches `state.json` for 48 h |
| `route_prs.sh` | "what PR needs what next" routing | **scheduler** (job DAG + capability match) | scheduler dispatches a real build/review lease |
| `review_prs.sh` | invoking the reviewer, gathering verdict | `code_reviewer` role + **reconciled sign-off** (I-9) | first SHA-bound sign-off minted from reconciled state |
| `codex_watch_needs_codex.sh` | claim-dirs, board-sync, sweep, poison-guardian | **fenced leases** (I-4) + escalation (I-6) + **Rung-3/4 liveness** | first lease survives a heartbeat cycle + a forced revocation |
| 2× launchd dispatchers + reviewer-watchdog | spawning/restarting agents | **pull-loop workers** dialing out (R2) | two workers (one build, one review) lease concurrently |
| per-token GitHub hammering | outbound writes on a shared token | **project-OUT** outbox + single installation identity (I-14) | `rateLimit.remaining` floor stops dropping |

The cutover discipline is **read-before-write, mirror-before-schedule**:

```
 PHASE 0  Flowbee dark-launched: reconcile-IN only. Scripts still own everything.
          Flowbee's mirror is compared to reconcile.go's state.json. No writes.
              │  (mirror agreement ≥ 48h)
              ▼
 PHASE 1  Flip the SCRIPTS to read Flowbee's mirror instead of polling GitHub.
          ── this alone kills the rate-limit storm: N redundant statusCheckRollup
             polls collapse to ONE reconcile-IN sweep. project-OUT owns writes.
              │  (rateLimit.remaining floor flat for 1 week — §13.4)
              ▼
 PHASE 2  Flowbee SCHEDULES: fenced leases, capability match, ADOPT-mode import,
          one build worker + one review worker, reconciled sign-offs, spec gate.
          route_prs.sh / review_prs.sh / codex_watch deleted as each lease path proves out.
              │
              ▼
 PHASE 3  Decision-gated (§14): either the human merge gate stays (ship lean),
          or it comes off (content-integrity + side-effect machinery becomes MVP).
```

Phase 1 is the single highest-leverage step: it removes the exact failure (≈5 loops + every worker poll on one shared token, 3 redundantly polling `statusCheckRollup`) that motivated the whole project, and requires **none** of the agent-orchestration machinery. If Flowbee shipped *only* Phase 1, it would already have paid for itself.

### 13.2 What ships in v1 vs. deferred to v1.1

The line: **v1 contains exactly the invariants whose absence reintroduces a failure the original pipeline already suffered, plus the minimum to run two workers across the SHA boundary.** The notation `[T1✚]` marks items that **become v1** if §14 removes the human merge gate and **stay deferred** if it stays — the swing set.

| Capability | v1 (MVP) | v1.1 / later | Invariant |
|---|:---:|:---:|---|
| reconcile-IN sweep (Domain-B facts) | ✅ | | I-1, I-3 |
| Webhooks-as-hints + HMAC + dedupe + write-ahead inbox | ✅ | | I-2 |
| project-OUT outbox (labels/checks/comments/draft↔ready/PR-open/merge-enqueue) | ✅ | | — |
| **Single GitHub App installation token** (drop multi-PAT pool) | ✅ | | **I-14** |
| Fenced exactly-once leases (atomic claim + epoch) | ✅ | | I-4 |
| Capability matching on **attested** tags; derived constraints (arch-lottery fix) | ✅ | | — |
| `max_attempts`/`max_bounces` → `needs_human`; `no_eligible_worker` alarm | ✅ | | I-6 |
| **ADOPT mode** (import open issues/PRs as mirrored-but-quiescent) | ✅ | | **I-16** |
| **Reconciled sign-off** (verdict derived from reconciled state, never `status:succeeded`) | ✅ | | **I-9** |
| Enforced anti-affinity (identity ✚ model_family ✚ lens, 4 terms) at lease time | ✅ | | I-10 |
| Spec flow (`spec_author` → `spec_review` GATE) before any SHA | ✅ | | — |
| Build flow, **batch-size-1** merges via merge queue (through `merger` stage) | ✅ | | I-5 |
| Branch protection on `main` as server-side backstop | ✅ | | I-8 |
| Flowbee performs all git writes; ephemeral creds; workers hold no token | ✅ | | I-7 |
| Per-job / per-flow **cost ceiling** (token/$ meter) | ✅ | | I-15 |
| Liveness: **Rung-3 + Rung-4 + minimal Rung-2 gate + two fast-paths** | ✅ | | I-13 |
| **Content-integrity gate** (path denylist + blast-radius + static checks) | `[T1✚]` | ✅ | **I-11** |
| **Epoch-namespaced side effects** (refs/CI-by-epoch/compensation) | `[T1✚]` | ✅ | **I-12** |
| **Unattended merge** (`self_merge` arm enabled) | `[T1✚]` | ✅ | I-9+I-11+I-12 |
| Multi-reviewer fan-out / quorum (`majority`/`any_veto`/`weighted`) | | ✅ | — |
| Merge-queue batch-size > 1 + `flowbee/review-valid@SHA` re-eval | | ✅ | I-5 (merge-queue fix) |
| SSE event stream (replace long-poll) | | ✅ | — |
| Full Rung-0 / Rung-1 liveness + adaptive priors | | ✅ | I-13 |
| Stacked-PR depth, HA, multi-repo, multi-tenant | | ✅ | — |

Two points the draft's §10 did not make: **the spec flow is v1, not v1.1** (Flowbee's real entry point is a chat that produces a spec, gated *before any SHA exists*; that gate is cheap — pure Flowbee state — and is the first place independence is enforced); and **ADOPT mode is v1, full stop** (without I-16, project-OUT would seize the board on first boot).

### 13.3 The smallest slice that still honors the non-negotiables

The **minimum** Flowbee runnable against live `russ` without violating a single non-negotiable invariant — Branch A, Phase 2, nothing more:

```
 SMALLEST HONEST FLOWBEE  (Branch A / human gate stays)
 ─────────────────────────────────────────────────────
  ✓ reconcile-IN sweep (I-1, I-3)        ✓ webhooks-as-hints, HMAC, inbox (I-2)
  ✓ project-OUT outbox + ONE installation token (I-14)
  ✓ ADOPT mode (I-16)                    ✓ fenced exactly-once leases (I-4)
  ✓ capability match + derived constraints (arch-lottery dead)
  ✓ spec_review GATE + lens anti-affinity (I-10)
  ✓ code_review GATE → reconciled sign-off (I-9)
  ✓ enforced anti-affinity at lease time, 4 terms (I-10)
  ✓ batch-size-1 merge via merger stage (I-5)   ✓ branch protection backstop (I-8)
  ✓ Flowbee does all git writes (I-7)    ✓ per-job cost ceiling (I-15)
  ✓ Rung-3 + Rung-4 + minimal Rung-2 + 2 fast-paths (I-13)

  ✗ handoff → HUMAN merges. self_merge arm DISABLED.   ← the human IS the I-11 gate
  ✗ NO content-integrity machinery (I-11 deferred)     ← premature while the human gates
  ✗ NO epoch-namespaced side-effects beyond fencing (I-12 deferred)
  ✗ NO multi-reviewer fan-out, NO SSE, NO HA, NO multi-repo
```

The MVP is "lean" by deferring exactly the two invariants (I-11, I-12) the human merge gate makes redundant — **and not one more**. Drop fenced leases, reconciled sign-offs, ADOPT mode, anti-affinity, or the single-identity token and you have not shipped lean; you have shipped the original pipeline's bugs with a new name.

### 13.4 The measurement gate (do this BEFORE choosing a branch)

Both branches share a prerequisite, and it is *not* optional: **land the interim rate-limit patch and measure `rateLimit.remaining` for a week before building anything heavier.** This is the cheapest de-risking and it informs the decision rather than presuming it.

**The interim patch** (deployable against the *current* scripts with zero Flowbee — defined here once; §8.4 cites this): (1) **de-dup the 3 redundant `statusCheckRollup` polls** onto `reconcile.go`'s existing `state.json` (the other loops read the mirror instead of GitHub); (2) **slow the clocks** on the remaining poll loops; (3) **add 403/`Retry-After` backoff** on the shared token. Then instrument and watch for a week:

| Signal measured | What it tells you | How it informs §14 |
|---|---|---|
| `rateLimit.remaining` floor (min over the day) | Is the rate-limit storm solved by the patch alone? | Healthy floor with *just* the patch ⇒ Branch A "ship lean" is well-supported. |
| review throughput vs. human-merge latency | Is the **human** the bottleneck, or verification? | Humans keep up ⇒ Branch A costs little. Human-merge is the jam ⇒ Branch B's value is concrete. |
| frequency of denylist-relevant diffs (CI config, lockfiles, Dockerfiles) | How often would the content-integrity gate fire? | High ⇒ Branch B's I-11 is load-bearing and worth building; low ⇒ the human gate is rarely exercised on dangerous paths. |
| build/review cost per job (token/$) | Does the cost ceiling (I-15) bind in practice? | Sizes the blast radius of an unattended (Branch B) runaway before enabling `self_merge`. |

The sequencing rule is blunt: **measure, then ship lean, then — and only with data in hand — decide whether the gate comes off.** Building I-11/I-12's heavy machinery *before* the measurement gate is the exact "move fast" mistake the invariants exist to prevent.

---

## 14. THE ONE DECISION — RESOLVED: Branch B (autonomous merge, no human gate)

> **RESOLVED — Branch B.** The MVP **may** merge without a human in the loop. The `self_merge` arm (§5.4) is the production posture; an approved + denylist-clear + blast-radius-consistent + CI-green-at-head job is merged by Flowbee with **no human between the verdict and the merge commit**. The toggle is **configurable** — `Policy.AllowSelfMerge` (`config.AllowSelfMerge`, env `FLOWBEE_ALLOW_SELF_MERGE`, `flowbee.yaml: allow_self_merge`) — and defaults to `false` in code as a conservative fail-safe, but the **production setting is `true`**. The architecture made this a policy flip, not a redesign (§14 "Why this is held open"); the flip is taken. The safety net is **deterministic, not a human**: the content-integrity gate (I-11), epoch-namespaced side effects (I-12), the reconciled SHA-bound verdict (I-9), and CI-green-at-the-integrated-head (I-5) — with auto-revert as named later hardening. Consequently **I-9, I-11, and I-12 are load-bearing on day one**, and §9.7's egress gap is a residual risk the operator consciously accepts (mitigated by running cross-box workers only on trusted hardware). The sections below record *why* the decision went this way; they are retained as the rationale, not as an open question.

This was the one decision the document deliberately deferred. It re-sorts the `[T1✚]` rows of §13.2 and determined what "MVP" means. **It has now been made by Sam: Branch B.**

> **The question:** at the code-review GATE, when `code_reviewer` returns `approved`, may Flowbee take the **`self_merge` arm** (§5.4) and merge to `main` with **no human between the verdict and the merge commit** — or must *every* approved job take the **`handoff`→human** arm?

The two branches are not "more features vs. fewer." They are **two different products with two different MVPs:**

```
                          ┌──────────────────── THE ONE DECISION ─────────────────────┐
                          │   May an approved job merge with NO human in the loop?      │
                          └───────────────┬────────────────────────────┬──────────────┘
                                          │ GATE STAYS                  │ GATE COMES OFF
                                          ▼                             ▼
            ┌─────────────────────────────────────────┐ ┌──────────────────────────────────────────┐
            │ Product = "a faster, safer human board"  │ │ Product = "agents that merge unattended"  │
            │ self_merge arm DISABLED in policy.       │ │ self_merge arm is the WHOLE POINT.        │
            │ Every approved job → handoff → human.    │ │ NOTHING but Flowbee stands between an LLM  │
            │ Content-integrity (I-11) + epoch-        │ │ verdict and main.                          │
            │ namespaced side-effects (I-12) are       │ │ ⇒ I-9 reconciled provenance, I-11 content │
            │ PREMATURE. The human IS the gate.        │ │ integrity, I-12 epoch side-effects ARE    │
            │ SHIP LEAN (§13.3) + measure first (§13.4)│ │ the MVP — not v1.1 polish.                │
            └─────────────────────────────────────────┘ └──────────────────────────────────────────┘
```

**Branch A — the human merge gate STAYS.** If a human reviews and clicks merge, the human *is* the content-integrity gate. An LLM reviewer that waves through a poisoned `.github/workflows` change is caught by the human before `main`. Building I-11's path-denylist + blast-radius + static checks and I-12's epoch-namespaced refs/`(job,epoch)` CI gating/compensation *first* is **premature optimization of a safety boundary you already have**. Ship lean (§13.3), measure first (§13.4). A perfectly respectable place for v1 to live indefinitely.

**Branch B — the gate COMES OFF.** The instant no human stands between an LLM's `approved` and a merge commit, the LLM reviewer's verdict is load-bearing in a way it never was, and a returned diff is *untrusted data* with a direct path to production:

- **I-11 (content-integrity)** is the *only* deterministic, non-LLM thing standing between a prompt-injected `eng_worker` and a malicious CI-config change. The path denylist is the **primary safety boundary**, not defense-in-depth.
- **I-12 (epoch-namespaced side effects)** stops being hygiene and becomes the thing preventing a revoked zombie from racing a live worker to the merge. Ack≠execution is no longer abstract: a stale epoch that force-pushes a branch now has an unsupervised path to `main`.
- **I-9 (reconciled-verdict provenance)** is what makes a "sign-off" mean something when no human re-derived it — the verdict *must* be Flowbee's own conclusion from reconciled GitHub state, because there is no human to catch a forged `status:succeeded`.
- **§9.7's egress gap** moves from "named post-v1 work" to a residual risk the operator must consciously accept.

In Branch B these are **the actual MVP**; Phases 0–2 become table stakes on the way to the real product.

**Why this is held open.** It is a risk-appetite and operational-readiness call, not an engineering one — and the architecture is deliberately constructed so **either branch is reachable without restructuring.** The `self_merge`/`handoff` branch point (§5.4) is a policy toggle, not a wiring change; flipping it disables one arm and promotes three invariants from deferred to MVP. The document's job is to make the cost of each branch legible, then get out of the way.

---

## 15. Correctness invariants (the canonical I-1 … I-16)

The single source of truth for the invariant IDs. They hold even at MVP; their removal reintroduces the exact wedge/double-merge/unreviewed-merge failures Flowbee exists to delete. The first eight carry the draft's §11 forward; the rest are clean-room additions.

**The determinism invariant (the load-bearing architectural commitment):**

- **I-0 — Determinism / replayability.** *flowbee-core is a deterministic, replayable function of persisted facts; all intelligence is a job.* The core packages (`internal/{engine,job,ledger,lease,scheduler,flow}`) read no clock, mint no IDs, draw no randomness, and import no LLM/GitHub package — time and IDs are injected as values; the same event log replays to the same `Decision` stream. Enforced mechanically by `tools/archcheck` (the deterministic-hash exception for `crypto/sha256`/BLAKE3 aside). Its removal makes the orchestrator unauditable and unreplayable. (§1.2-build, §3.6)

**Carried forward (draft §11):**

- **I-1 — reconcile-IN is ground truth for Domain B; webhooks are hints.** No webhook is authority; gaps (delivery high-water-mark) force a sweep. (§3.3, §8.1)
- **I-2 — HMAC-verify every webhook, dedupe by `X-GitHub-Delivery`, write-ahead inbox** before acting (crash-replay safe; the endpoint is internet-reachable). (§8.1.3)
- **I-3 — SHA-monotonic gating + terminal-SHA guard.** Ignore any event older than recorded head SHA / `updated_at`; a merged/settled job is never re-dispatched by a late or replayed event. (§8.1.5)
- **I-4 — fenced, exactly-once leases:** atomic claim (`UPDATE … WHERE state='ready'`) + partial unique index "one active lease per job" + `lease_epoch` rejecting stale mutations. (§6.3)
- **I-5 — serialized merges; CI-green AND review-valid bound to the exact `(head_sha, base_sha)`.** Any base/head move supersedes the verdict and re-arms review + CI. (§6.2.4, §8.5)
- **I-6 — `max_attempts` / `max_bounces` → `needs_human`; `no_eligible_worker` alarm.** Bounded retry, no infinite poison loop, no silent starvation. (§6.6, §6.7)
- **I-7 — Flowbee performs all git writes** with ephemeral, least-privilege creds; workers never hold the installation token; promotion of worker output goes only through epoch-namespaced refs. (§3.5, §9.4)
- **I-8 — branch protection on `main` as a *structural* server-side backstop:** no direct/force push, required checks, merges only via Flowbee's protected path. It is a **structural** gate ("nothing reaches `main` except via Flowbee's check"), **not** the reviewer-≠-author arbiter — Flowbee owns actor identity and enforces review independence at lease time (I-10), since to GitHub every Flowbee action shares one identity. (§9.6, §8.3)

**Clean-room additions:**

- **I-9 — Reconciled-verdict sign-off.** A sign-off (spec-review gate, code-review gate) is a **tamper-evident Flowbee record derived from reconciled GitHub state** (build) or the reviewed spec bytes addressed by `spec_content_hash` (spec), never a worker self-reported `status: succeeded`. (§5.5, §11.5)
- **I-10 — Independence / anti-affinity is enforced, not advisory.** The canonical four terms — `eng_worker.identity != code_reviewer.identity` ∧ `eng_worker.model_family != code_reviewer.model_family` ∧ `spec_author.lens != spec_reviewer.lens` ∧ `code_reviewer.identity != merger.identity` — enforced at lease time (§6.3.1); reviewer context stripped of untrusted PR/issue prose (judge the diff). (§5.5, §9.3)
- **I-11 — Content-integrity gate.** A returned diff is **untrusted data.** Before auto-merge it must pass a **path denylist** (CI config, `.github/workflows`, lockfiles + postinstall, Dockerfiles, secrets, Flowbee's own source → forced human gate), a **declared blast-radius** check, and **deterministic non-LLM static checks**. No LLM verdict alone clears this gate. (§9.2)
- **I-12 — Epoch-namespaced side effects (ack ≠ execution).** Every externally-visible action is idempotent against re-dispatch: epoch-namespaced refs promoted only post-validation, CI gated on `(job, epoch)`, explicit compensation on revocation, no worker-supplied PR number. (§3.5, §6.5)
- **I-13 — Two-rung kill rule.** A kill (lease revocation for a presumed-stalled agent) requires **two independent liveness rungs agreeing, at least one Rung-2 (external) or Rung-3 (clock)** — never two worker-reported rungs. "Worker partitioned" is distinguished from "agent stalled"; the reconnecting zombie is handled by fencing (I-4). (§10.3)
- **I-14 — Single-identity ToS constraint.** Outbound GitHub identity resolves to **one** ToS-clean identity — a **fine-grained, repo-scoped PAT for the single-operator default** (reconcile-first makes the 5k/hr budget a non-issue), or **a GitHub App installation token at org/multi-repo scale**. The multi-PAT/multi-account "multiply the buckets" *pool* is **dropped** — a ToS-suspension vector. Stated as a constraint, not a tuning knob. (§8.3, §9.4)
- **I-15 — Per-job / per-flow cost ceiling.** Token/$ metering with an enforced ceiling per job and per flow, alongside the GitHub-budget meter. Exceeding the ceiling escalates; it does not silently overspend. (§6.7, §12.6)
- **I-16 — ADOPT-mode bootstrap.** First run against a live repo imports existing open issues/PRs as mirrored-but-quiescent; only work opted-in via label / watermark is scheduled. Flowbee never seizes human-owned in-flight work, and project-OUT does not reassert renderings on adopted-quiescent objects until opt-in. (§12.7)

> **RESOLVED (§14): Branch B — the gate comes off.** May the MVP merge without a human in the loop? **Yes**, configurable via `Policy.AllowSelfMerge` (production `true`). Therefore **I-9 + I-11 + I-12 are the actual MVP**, not v1.1 polish — the deterministic content-integrity / epoch-side-effect / reconciled-provenance machinery is what stands between an LLM verdict and `main`.

*(Capability attestation — "probed/attested, not self-declared" — is a hard requirement carried from the original spec §8 and enforced in §7.2/§9.4.1, but is not assigned a numbered invariant; it is a property of the worker protocol's trust boundary.)*

---

## 16. Biggest risks (design around these)

| # | Risk | Why it bites | Mitigation (where) |
|---|---|---|---|
| 1 | **Webhooks-as-sole-truth → board drift** (highest in the draft) | webhooks are lossy/replayable/forgeable | reconcile-IN is ground truth; webhooks are hints (I-1, §3.3, §8.1) |
| 2 | **Forged/replayed events → unreviewed merge** | internet-reachable endpoint | HMAC + dedupe + write-ahead inbox + SHA-monotonic gating (I-2, I-3) |
| 3 | **Worker compromise** (untrusted agents, broad local privilege, untrusted prose) | prompt injection + permission bypass | credential classes + content-integrity gate + diff-only reviewer context + branch-protection backstop (§9) |
| 4 | **Stale approval on base/head move** (stacked PRs, merge-queue integrated head) | the queue re-runs CI but not review | verdicts bound to the SHA pair; `superseded` re-arm; `flowbee/review-valid@SHA` re-eval (I-5, §8.5) |
| 5 | **Ack ≠ execution / zombie workers** | fencing doesn't reach git/CI/GitHub | epoch-namespaced refs + `(job,epoch)` CI + explicit compensation (I-12, §3.5, §6.5) |
| 6 | **Crash-consistency / SPOF** | single process, v1 | reconstruct-on-boot, idempotent re-checkable external actions, WAL+replication, launchd KeepAlive (§12.4) |
| 7 | **Liveness false-positives** (kill a healthy 40-min E2E or a long reasoning step) | cheap signals are gameable, un-gameable ones are slow | 5-rung ladder + two-rung kill rule + abstain semantics + CI-tolerance extension (I-13, §10) |
| 8 | **Verification becomes the bottleneck** | unattended merge outruns trust | cap WIP via the human merge gate (Branch A) OR ship the full content-integrity machinery (Branch B) — §14 decides |
| 9 | **ToS suspension from multi-PAT buckets** | sock-puppeting rate limits | single installation identity; the pool is dropped (I-14, §8.3) |
| 10 | **First-boot stampede over live human PRs** | project-OUT seizes the board | ADOPT mode: mirrored-but-quiescent + opt-in (I-16, §12.7) |
| 11 | **Worker-host data exfiltration** | the host owns its own egress | bounded blast radius (no token, one-epoch write key); egress confinement is the first post-v1 item (§9.7) |
| 12 | **Cost runaway** (a spinning agent burns tokens) | the draft metered only the GitHub budget | per-job/per-flow token/$ ceiling → escalate (I-15, §12.6) |

---

## Appendix: what it replaces

| `russ` artifact | Flowbee successor |
|---|---|
| `scripts/pipeline/reconcile.go` | the **reconcile-IN** sweep (Domain-B ground truth, §8.1) |
| `route_prs.sh` + `review_prs.sh` | the **scheduler + flow engine** (job DAG, capability match, gated flows, §5–§6) |
| `codex_watch_needs_codex.sh` (claim-dirs, board-sync, sweep, poison-guardian) | the **worker registry + fenced leases + escalation + liveness ladder** (§6, §7, §10) |
| the two launchd dispatchers + reviewer-watchdog | **pull-loop workers** dialing out + the single control plane (§4, §7) |
| per-token GitHub hammering | the **single installation identity + reconcile-first + project-OUT outbox** (§8, I-14) |

What is genuinely *new* (no `russ` antecedent): the **spec-review gate and spec flow** (§11), the **two-domain reconciliation model** (§3), the **content-integrity gate** (§9.2), the **scoped-read provisioning credential class** (§7.4, §9.4), **ADOPT mode** (§12.7), and the **per-job/per-flow cost ceiling** (§12.6).

The original working-name draft ("Foreman") additionally floated a multi-PAT identity pool and a single build→review→merge flow rooted at "build an issue"; both are **superseded** here — the pool by the single installation identity (I-14), and the single flow by the two gated flows joined at the SHA boundary (§5).
