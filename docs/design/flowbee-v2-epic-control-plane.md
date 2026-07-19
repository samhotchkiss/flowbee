# Flowbee v2: durable epic control plane

**Status:** adversarial revision re-signed `GO_WITH_CHANGES`; implementation is gated
on the additive pre-activation fixes listed in §19

**Version:** 0.3

**Date:** 2026-07-18

**Repository baseline:** `5ee339a8647c8c32a6b8d0696f4fc7c8ff7deee7`

**Scope:** the next-stage architecture and phased build plan; no implementation is
authorized merely by merging this document.

**Build gate:** Fable's follow-up review returned `GO_WITH_CHANGES` and locked the
architecture. The incident slice may be implemented only with the §8.9 registry,
review-verdict, dedup-revisit, digest, migration, and dead-letter gates included
before their respective activation points.

## 0. Purpose and relationship to the existing design

This document turns the current epic lane into the durable operating system for the
whole engineering loop: human intent, epic admission, build, CI, independent review,
rework, merge, conflict handling, cleanup, and escalation.

It is a versioned successor to
[`docs/design/epic-lane.md`](./epic-lane.md), not a rewrite of its history. The
existing document remains the source for mechanisms already designed or shipped:
seat-bound launches, scope exclusion, account windows, the supervision ticker,
attention items, epoch-fenced leases, anti-affinity, and the launch ladder. This v2
plan changes the domain boundary in several important ways:

1. **An epic is Flowbee's only unit of owned work.** Issues can inform an epic, but
   Flowbee does not own, adopt, or dispatch one-off issues or arbitrary pull requests.
2. **Ownership starts at epic admission.** A branch and pull request are later output
   artifacts of an already-owned epic. They are never intake or adoption signals.
3. **The review obligation is created with the epic.** It cannot be created by a
   conversational dispatcher after a build finishes.
4. **The database is the source of truth.** Tmux observation, `tmux-send` actuation,
   and agent sessions project durable intent; they do not remember the pipeline on
   Flowbee's behalf.
5. **The dashboard is the normal human workspace.** Tmux remains agent transport and
   a debugging fallback, not the primary operator interface.
6. **Projects become first-class.** A project is not a synonym for a repository; one
   project can own several repositories and many epics.
7. **Product intent promotes automatically.** Once a work request is sufficiently
   defined and its required decision gates are satisfied, the Interactor routes it to
   its paired Orchestrator, which submits the resulting epic to Flowbee. The human is
   never responsible for saying "send this to Flowbee."

Where this document conflicts with the current epic-lane plan, this document is the
v2 target. In particular, the older references to automatically adopting labeled PRs
or running a parallel one-off issue lane are compatibility history, not the v2
ownership model.

The reliability requirement that motivated this plan is concrete:

> Once Flowbee admits an epic, every required next action must be reconstructible
> from durable state after any process, dispatcher, Grok, Codex, Interactor, terminal,
> or Tmux Driver interruption.

## 1. Canonical topology and responsibility boundaries

The intended topology treats Flowbee as the durable plane through which work is
admitted, scheduled, observed, actuated, and reported—not as a peer command box:

```text
Human ──▶ one dashboard / Needs You / project conversations
                         │
             ┌───────────┴───────────┐
             ▼                       ▼
 project A Interactor       project B Interactor       (Claude/Fable)
             │ 1:1                  │ 1:1
 project A Orchestrator     project B Orchestrator      (Codex)
             └───────────┬───────────┘
                         │ clients submit epics and decisions through the plane
┌────────────────────────▼──────────────────────────────────────────────────┐
│                     ONE LOGICAL FLOWBEE PLANE                             │
│ durable DB/ledger · scheduler · gates · reconcilers · alerts · read model │
│ bounded operational agent (Grok) · observer · tmux-send actuator          │
│                                                                          │
│       shared builders                         shared reviewers            │
│       isolated worktrees/sessions             independent leases          │
└──────────────────────────────────────────────────────────────────────────┘
```

The named families are the **default deployment profile**, not domain-schema enums:
Claude/Fable for Interactors, Codex for Orchestrators/builders, and Grok for the
operational/review role. Durable records use configured role capabilities, identity,
and model family. The hard rule is independent builder/reviewer identity and family;
a future provider can fill a role without a schema migration.

### 1.1 Human

The human owns product authority and exceptional authorization. The human should be
able to understand the portfolio, answer questions, approve or reject plans and
designs, and resolve true escalations without attaching to a terminal.

The human does **not** babysit handoffs, notice idle seats, manually ask whether work
is building, remember which review needs to happen next, or tell an Interactor to
push an already-authorized request through the Orchestrator and Flowbee.

### 1.2 Interactor — Claude/Fable, project-scoped

Each project has one logical Interactor. It owns the focused conversation with the
human: framing choices, presenting tradeoffs, gathering answers, and communicating
outcomes. It is the human-facing judgment layer, not the build dispatcher.

The Interactor also owns automatic **intent promotion**. It converts an executable
human request into a versioned work-intent artifact. If definition or approval is
missing, it creates the appropriate typed question/decision. Once the intent is
defined and every required gate is satisfied, it durably routes the intent to the
paired Orchestrator without waiting for another human prompt. A direct product request
can authorize creation of a work intent; it does not silently satisfy a separate
high-consequence plan, design, or exception approval gate.

An Interactor can be restarted, compacted, or replaced. Open questions and prior
answers, unpromoted work intents, and promotion acknowledgements must survive because
they are durable records, not transcript-only memory. On every incarnation it loads
the current actor contract and drains pending project intents automatically.

### 1.3 Orchestrator — Codex, project-scoped and 1:1 with an Interactor

The Orchestrator owns product logistics for one project:

- claiming ready work intents from its paired Interactor without a human nudge;
- priorities, sequencing, and dependencies;
- grouping one or more issue references into a coherent epic;
- authoring or refining the epic contract;
- selecting the delivery repository and declared repository set;
- routing product questions to the Interactor;
- submitting a complete epic to Flowbee idempotently and acknowledging the admitted
  epic ID back through the route.

The Orchestrator does not own build continuity after admission. It must not need to
remember to dispatch review or merge a PR.

### 1.4 Flowbee — one logical authority

Flowbee consists of two cooperating parts:

- the **deterministic control plane**, which owns durable state, scheduling,
  reconciliation, leases, gates, idempotency, alerts, audit, and external effects;
- the **Grok operational layer**, which performs bounded operational judgment and
  supervises build logistics under durable assignments.

Flowbee owns:

- epic admission and placement;
- branch/worktree/session provisioning;
- build and review routing;
- CI and GitHub fact reconciliation;
- cross-family reviewer anti-affinity;
- review rejection and rebuild loops;
- conflict resolution, merge, and cleanup;
- capacity allocation across projects;
- self-healing and escalation.

"One logical authority" does not mean one immortal process or Grok session.
Processes are replaceable, but Phase 0 enforces exactly one writer with an exclusive
database lock; a replacement refuses readiness until the prior writer releases it.
Operational sessions may overlap only as fenced claimants. A process death can delay
work by at most the configured reconciliation/lease window; it cannot erase work.

### 1.5 Build and review sessions

- **Builders are Codex sessions in the default profile.** They receive a bounded epic
  contract and work on the epic's isolated branch/worktree.
- **Reviewers are Grok sessions in the default profile.** They independently inspect
  and run the change, bound to the exact artifact head they review.
- A reviewer never shares the builder's model family. This is a hard lease predicate,
  not a preference with a same-family fallback.
- A build or review session reports observations and results. It does not choose the
  next pipeline transition.

### 1.6 Tmux Driver boundary

Sam's page-12 markup changes the product boundary: the Tmux Driver now owns the
mechanical terminal-control substrate, including typing text/keys, handling approved
permission/dialog/credential prompts, implementing or embedding `tmux-send`, and
managing leases, draft stashing/restoration, verified send receipts, and control APIs.
These capabilities remain deterministic, bounded, audited, and subordinate to
Flowbee's durable action/state machine; moving them into Driver scope does not give
the Driver product judgment, ownership, merge authority, or permission to interpret
arbitrary pane text as intent.

The Driver may contain observer, actuator, session-control, lease, and receipt
adapters behind one versioned boundary. It must still use remote-session facts,
`expect_pane_instance`, action epochs, closed-template payloads, and durable
acknowledgements. §7 defines those capabilities and their trust limits.

Two marked exclusions remain deliberately open questions for Sam, not assumptions:
whether observation through a local outer SSH proxy may ever be authoritative, and
whether a public Internet listener belongs in the Driver boundary. Until answered,
remote tmux facts remain authoritative and no public listener is part of the design.

## 2. Ownership model and vocabulary

### 2.1 Project is not repository

A **project** is the product and coordination boundary. It has one Interactor, one
Orchestrator, policies, priorities, repository membership, and a project workspace in
the dashboard.

A **repository** remains the GitHub/CI/merge policy boundary. Branch protection,
required checks, base branch, pull request, merge queue, and deployment rules are
evaluated per repository.

The default project can own one or many repositories. Every epic belongs to exactly
one project and declares an explicit repository set. In the initial v2 policy it also
selects exactly one `delivery_repo`; that repository gets the epic's one delivery
branch and one pull request. Other declared repositories are context or dependency
inputs unless a later cross-repository delivery policy is deliberately designed and
enabled.

### 2.2 Epic is the sole Flowbee ownership unit

An epic is a coherent delivery contract submitted by an Orchestrator. It may contain:

- one issue reference;
- several related issue references;
- no pre-existing issue, when the contract originates from planning.

Issue references are metadata and evidence. They do not independently enter a
Flowbee queue and never determine ownership.

Admission creates a durable epic identity before any session is launched. That
identity owns the branch name, delivery obligation, required review, project route,
and cleanup obligation from the beginning.

### 2.3 Branches and PRs are output artifacts

The default contract is one epic → one delivery branch → one pull request. Flowbee
chooses and records the branch at admission, normally `epic/<slug>` within the
delivery repository. A later GitHub pull request is associated only when all of the
following match:

- project and delivery repository;
- the exact branch recorded at admission;
- a same-repository head, not a lookalike branch from a fork;
- the current branch tip/head SHA;
- no conflicting already-bound PR.

Labels, issue references, PR titles, commit authors, and a green CI result are not
ownership proofs. An unrelated green PR must remain invisible to the epic dispatcher.

Two open PRs for one epic branch are an ambiguity requiring an alert; Flowbee must
not guess. A PR number becomes immutable for the delivery after binding unless an
audited recovery operation explicitly replaces a closed, unmerged artifact.

### 2.4 One delivery by default

The one-branch/one-PR default keeps review, verdict binding, merge, rollback, and
cleanup attributable. A cross-repository epic that needs multiple write branches or
PRs is not silently approximated. It is either split into dependent epics or handled
under a future, explicit multi-delivery policy with its own barrier semantics.

## 3. Current state and the production gap

The repository already contains most of the reliability primitives, but they stop at
the seam between the session-per-epic lane and the older jobs pipeline.

### 3.1 What is already durable

- `Store.AddEpicRun` writes an `epics` row in `state='launching'` before tmux launch.
  It atomically binds the selected seat, account, host, branch, scope, and
  `builder_model_family`.
- Seat concurrency permits multiple simultaneous epics on one host while the
  per-epic worktree and scope gates prevent them from squashing each other.
- The epic supervision ticker ingests status from the epic branch and tracks the
  builder session.
- The generic jobs pipeline has durable `review_pending → code_review` leases,
  epoch fencing, SHA-bound verdicts, no-eligible-worker timers, CI reconciliation,
  merge gates, and reviewer anti-affinity.
- `attention_items` is a durable, deduplicated, epoch-fenced operator queue.
- The dashboard and SSE feed already expose epic/session/account facts, though not
  the missing handoff.

### 3.2 What is not durable

The session-per-epic lane has no admission-time delivery/review record. Builder
completion and pull-request creation can be observed, but the instruction to create
or wake the review is still dependent on a dispatcher/Grok session completing the
next conversational step.

`docs/design/epic-lane.md` commits to a completion-triggered cross-family review
handoff in its Phase 8, but that handoff is not implemented. The generic PR adoption
path cannot fill the gap safely: it treats a PR as the ownership input and requires
an opt-in label. That is the opposite of the v2 model.

The canonical epic dashboard also reads epic session state without a linked delivery
state. A builder-terminal epic can therefore look completed while its green PR has no
review verdict and no reviewer.

### 3.3 Incident: 2026-07-17/18

Two pull requests in the `russ` repository, #4950 and #4951, were open and CI-green.
Neither had a review verdict or an active reviewer. Review capacity was idle, with
substantial headroom. A usage dialog interrupted the dispatcher after build
completion but before review dispatch.

The result was a silent, indefinite stall:

- the review obligation was not durable;
- restart/resume had nothing authoritative to replay;
- no reconciler recognized the missing handoff;
- no alert or dashboard state surfaced it;
- unmerged PRs held back effective build throughput;
- the incident was found only when the human asked whether anything was building.

This is not reviewer saturation. It is a lost state transition.

### 3.4 Failure class, not one-off patch

The durable fix must close every equivalent crash window:

1. process stops after epic admission but before session launch;
2. builder finishes but artifact observation has not run;
3. PR/CI facts are committed but review materialization is interrupted;
4. review task is committed but Tmux Driver delivery is interrupted;
5. prompt is submitted but the delivery acknowledgement is not committed;
6. reviewer dies after claiming;
7. reviewer rejects, but the builder steer is interrupted;
8. approval is committed but merge projection is interrupted;
9. merge succeeds but cleanup or terminal projection is interrupted.

At each seam, the preceding durable state must imply the missing next action so a
fresh process can converge.

## 4. Hard invariants

The implementation is correct only if all of these remain true.

1. **Admission atomicity.** Committing an epic also commits one delivery record and
   one required review obligation. A failed admission commits none of them.
2. **Epic-only ownership.** Flowbee never infers ownership from an arbitrary issue,
   label, branch, PR, review request, CI result, or commit author.
3. **One default delivery.** At most one live branch/PR binding exists per epic in v2.
4. **Durable next action.** Every non-terminal pipeline state has a deterministic
   next action or a durable, visible blocking reason.
5. **No session memory dependency.** Killing any Interactor, Orchestrator, Grok,
   Codex, Tmux Driver, or Flowbee process cannot delete desired state.
6. **Idempotent projection.** Replaying a reconciler or action executor cannot create
   a second review, duplicate a prompt, merge twice, or clean another epic's branch.
7. **Fenced execution.** Build/review/action writes carry epochs; stale actors lose.
8. **Artifact-bound judgment.** CI and review approval bind to the exact head/base
   SHAs and artifact version. Any relevant move supersedes the verdict.
9. **Independent review.** The reviewer identity and model family differ from the
   builder. No eligible reviewer means a visible wait and alert, never self-review.
10. **Reconcile facts, do not trust claims.** GitHub/CI/merge facts come from the
    reconciliation plane, not agent prose or a worker's success response.
11. **Tmux is transport.** Pane content is an observation, and a send receipt is not a
    workflow transition unless the corresponding durable action is acknowledged.
12. **Project isolation.** Every owned object and route carries `project_id`; one
    project's fault or quota incident cannot globally stall unrelated projects.
13. **Fair shared capacity.** Project weights may change share, but sustained eligible
    work in one project cannot starve forever behind another.
14. **Human decisions are version-bound.** Approval applies only to the exact plan,
    design, artifact, or authorization hash presented to the human.
15. **Dashboard truth is durable truth.** The human-facing state is projected from the
    same records the scheduler/reconciler use, not an optimistic UI-only status.
16. **Alert delivery is durable.** Recovery and its alert are either committed
    together or a durable `alert_pending` bit guarantees later emission.
17. **No silent terminal state.** `complete` means reviewed, merged, verified as
    required, and cleaned up—not merely "the builder said done."
18. **No human plumbing transition.** Defined, authorized work promotes from
    Interactor to Orchestrator to Flowbee automatically. The human may make product
    decisions or explicitly pause/cancel work, but never has to remind actors to use
    the system.
19. **Actor contracts are loaded, versioned, and testable.** Every actor incarnation
    acknowledges a compatible role/recovery contract before receiving work. No actor
    is expected to remember the architecture from an old transcript or a human
    reminder.
20. **Exactly one writer.** A process-level exclusive database lock and fenced action
    claims enforce one control-plane writer; `MaxOpenConns(1)` is not treated as a
    cross-process lock.
21. **Total progress contract.** Every delivery state and CI substate has one
    registered next action or explicit paused/held state, a due clock, an owner, and
    an alert path. The invariant is generated and property-tested, not reviewed by
    inspection alone.
22. **Review-family availability at admission.** A build family is eligible only when
    the configured fleet contains a distinct review family; same-family fallback is
    forbidden even when that means admission remains visibly held.

## 5. State model

The v2 model deliberately separates five axes that the current dashboard conflates:

1. **builder/session state** — whether the Codex session is launching, running,
   blocked, or finished;
2. **delivery state** — where the epic's branch/PR is in CI, review, merge, and
   cleanup;
3. **action state** — whether a required external or tmux effect is pending,
   delivering, acknowledged, retryable, or dead-lettered;
4. **decision state** — whether a human question/approval is open, answered,
   deferred, superseded, or resolved;
5. **work-intent state** — whether a product request is being defined, waiting for a
   gate, ready for its Orchestrator, being packaged, submitted, or admitted.

No single overloaded `epics.state` should encode all five.

### 5.1 Builder/session lifecycle and affinity

The target builder-affinity lifecycle is:

```text
pending → launching → running ↔ blocked
                         │
               builder work complete
                         ▼
             parked (no live agent pane)
                         │
                  review rejection
                         ▼
                    relaunching ──▶ running
                         │
                    merge/abandon
                         ▼
                      cleaning → closed/abandoned
```

The worktree, branch, builder identity, and declared scope affinity survive while the
epic is parked for CI and review. The live agent pane does **not** survive: parking
tears down both the authoritative remote agent session and any local attach proxy.
This is what makes releasing the physical compute slot truthful. A rejection performs
a fenced relaunch under a new session incarnation against the same worktree, branch,
and builder identity after capacity is reacquired.

Seat occupancy counts physical resident sessions only. Persistent worktree/branch/
scope affinity is a separate reservation and cannot be interpreted as a free scope.
The `parked` affinity is a valid rework target, but delivery means "relaunch then
send," never "steer a pane assumed to still exist."

Scope/conflict protection remains reserved until merge or explicit abandon cleanup;
otherwise a parked epic could be silently overlapped by a new build before its review
returns. Only cleanup after a reconciled merge/abandon closes the session, removes the
worktree, releases the scope, and deletes the branch when safe.

The current `epics.state` values `done|achieved` continue to be emitted and consumed
unchanged during the incident slice. Builder affinity moves to a new
`epic_deliveries.builder_affinity_state` projection; it is **not** implemented by
remapping `epics.state` to `parked`. This preserves the existing 14-day epic-PR
detection window and merge-safety reads in `ListEpicRunsForRepo`, `EpicForHeadSHA`,
the review-lease brief, `KindEpicFinished`, and dashboard completion helpers. The raw
builder claim remains evidence, while the combined delivery projection prevents it
from presenting an unreviewed epic as delivered.

### 5.2 Delivery lifecycle

The target delivery state machine is:

```text
admitted → building → awaiting_artifact → awaiting_ci
                    └─ owned PR observed ───────┘
                                             │
                 CI red ─▶ rebuild_in_flight ├─ real green + non-draft
                              │              ▼
                              └──────▶ awaiting_review_dispatch
                                             │
                                             ▼
                                        review_queued
                                             │
                                             ▼
                                          in_review
                                      /                  \
                              request changes          approve
                                    ▼                     ▼
                           changes_requested        merge_queued
                                    │                     │
                                    ▼                     ▼
                           rebuild_in_flight           merging
                                    │              /              \
                                    ▼       conflict_resolution   merged
                               awaiting_ci             │             │
                                                      ▼             ▼
                                                 awaiting_ci  cleanup_pending
                                                                    │
                                                                    ▼
                                                                 complete

head/base advance while awaiting review, reviewing, or merging
  → cancel old-head action/lease/verdict → awaiting_ci
```

`paused` and `needs_human` are hold overlays, not terminal dead ends. They carry
`return_state`, owner, reason, entered/due clocks, and explicit resume/abandon edges;
all normal stall predicates exclude them. `abandoned` is reachable from every
non-terminal state and is terminal. Abandon atomically clears `review_required`,
cancels/fences current actions and leases, and closes or drafts the bound unmerged PR
according to repository policy so a later green fact cannot resurrect it.

Key transition rules:

- Admission creates `admitted` and `review_required=true` in the same transaction as
  the epic row.
- Launch acknowledgement moves `admitted → building`.
- An owned PR can be observed while the builder is still active. Its facts are
  recorded, but review eligibility requires an open, same-repository, non-draft PR,
  a current branch head, and real required CI success.
- `awaiting_review_dispatch` is a real durable state, not a computed absence. It is
  entered before an external review send can occur.
- `review_queued` means a durable review execution record/action exists but no
  reviewer holds its live lease.
- `in_review` requires a live fenced reviewer lease. A GitHub review request alone is
  not sufficient.
- A rejection creates a new review round only after the builder produces a new head.
  The prior verdict remains immutable history but cannot authorize the new head.
- `rebuild_in_flight → awaiting_ci` and `conflict_resolution → awaiting_ci` are the
  only forward edges after content changes. Neither can reach merge without fresh CI
  and a new independent SHA-bound verdict.
- Any head/base advance in `awaiting_review_dispatch|review_queued|in_review|
  merge_queued|merging` cancels—not retries—the old-head action, fences the old lease
  and verdict, resets review eligibility, and returns to `awaiting_ci`. A merge gate
  response of `newer artifact version` is terminal for that merge action.
- `merged` comes only from reconciled GitHub facts including the merge commit.
- `complete` additionally requires configured post-merge checks and cleanup effects
  to be acknowledged.
- Every state transition sets `state_entered_at`, derives `state_due_at` from the
  registered policy for that state, and appends a ledger event. A reconciled fact
  advance updates `fact_progress_at` even when the state does not change.

### 5.3 Artifact version and review round

Every branch-head change increments `artifact_version`. Every review attempt has a
`review_round` and binds to:

```text
(epic_id, delivery_repo, pr_number, artifact_version, head_sha, base_sha)
```

A reviewer lease and verdict carry all six values plus their lease epoch. A head/base
change fences the old reviewer and supersedes the old verdict. Replaying an old
approval returns a conflict and cannot move the delivery to `merge_queued`.

### 5.4 Durable action lifecycle

Every Tmux Driver, GitHub, merge, and cleanup effect is represented by a durable
action:

```text
pending → delivering → acknowledged
             │   │
             │   └─ uncertain outcome → verifying → acknowledged | pending
             └───── definite failure → pending (backoff) | dead_letter
```

An action's deduplication key is stable for one intended effect, for example:

```text
<project_id>:<epic_id>:dispatch_review:<head_sha>:<base_sha>
<project_id>:<epic_id>:builder_rework:<rejected_head_sha>:<current_head_sha>
<project_id>:<epic_id>:merge:<approved_head_sha>:<approved_base_sha>
<project_id>:<epic_id>:cleanup:<merge_commit_sha>
```

Content-bearing deduplication is always keyed by immutable SHAs; artifact version and
review round are display/ordering fields, never the uniqueness authority. Live,
acknowledged, and dead-lettered effects retain full-history uniqueness. An action
cancelled specifically because its head/base was superseded is marked
`cancelled_superseded` and excluded from the live dedup index, allowing an
H1→H2→H1 revisit to re-derive a healthy H1 action without duplicating a live effect.
A genuinely new head/base necessarily derives a new key even if a projection counter
missed an increment.

The state transition that requires an effect and insertion of its action row happen
in one database transaction. The executor may die at any later point; another
executor can resume from the action row.

### 5.5 Human decision lifecycle

Typed human requests use:

```text
open → viewed → answered/approved/changes_requested/deferred
  │                    │
  ├─ underlying version moved ─▶ superseded
  └─ no longer relevant ────────▶ cancelled
```

`viewed` is an acknowledgement, not a resolution. `deferred` must carry either a
`defer_until` time or a durable condition. A new version of the subject supersedes
the old request and creates or updates the current one; it never silently transfers
an approval to changed content.

### 5.6 Work-intent promotion lifecycle

Human conversation can create executable intent, but the promotion path is explicit
and durable:

```text
captured → defining ── missing answer/approval ──▶ awaiting_decision
               ▲                                      │
               └──────────── answer/change ────────────┘
               │
               └─ definition + required gates satisfied
                                      ▼
                         ready_for_orchestrator
                                      ▼
                              orchestrating
                              /           \
                   product question     epic ready
                         │                  ▼
                  awaiting_decision      submitting
                                            ▼
                                         admitted
```

`cancelled` and `superseded` are audited terminal states. A retryable delivery or
agent failure remains in the current non-terminal state with a durable action and
reason.

The Interactor, not the human, moves a fully defined and authorized intent to
`ready_for_orchestrator`. That transition atomically creates the Orchestrator delivery
action. The Orchestrator claims it, produces a versioned epic contract, and submits it
with an idempotency key derived from work-intent ID and version. Flowbee admission
links the resulting epic ID and completes the acknowledgement chain. If the Interactor
or Orchestrator restarts at any point, it resumes from this record. There is no manual
"push," "ship," or "send to Flowbee" state.

## 6. Durable data model

Migration numbers are allocated only under the merge-time discipline in §13.1. For
this work the next eligible number is `0032`; the unlanded historical reservations
`0029` and `0030` are never backfilled.

### 6.0 Epic identity and admission key

`epics.id` becomes the opaque surrogate identity. New rows use a ULID; existing IDs
remain valid opaque legacy identities. Human/file slugs move to `epics.slug`, with
`UNIQUE(project_id, slug)`, so two projects can reuse a slug without sharing an epic.
No delivery, artifact, action, decision, or work-intent foreign key uses a slug.

Every admission carries `admission_key`. A direct client must persist and retry the
same stable key; a regenerated slug, filename, or random key is a caller error and
can double-admit. Flowbee records that key for audit and rejects an absent direct key.
A work-intent admission derives it from `(project_id, work_intent_id, intent_version)`.
The store enforces `UNIQUE(project_id, admission_key)` and, when a work intent exists,
`UNIQUE(project_id, work_intent_id, intent_version)`. `AddEpicRun` performs lookup,
contract-hash comparison, epic/delivery/review-obligation insertion, and
`work_intents.admitted_epic_id` update in one serialized transaction. A retry with the
same key returns the existing epic; the same key with a different contract hash is a
conflict. Slug regeneration, a midnight boundary, or a lost response can never admit
a second epic.

### 6.1 `epic_deliveries`

One row is created for every admitted epic. The initial v2 implementation is 1:1,
with opaque surrogate `epic_id` as the primary key. This structurally enforces the
one-delivery default.

Recommended fields:

| field | purpose |
| --- | --- |
| `epic_id` | opaque primary key and FK to `epics.id`; never a slug |
| `project_id` | durable project route; backfilled to the default project |
| `delivery_repo` | repository that owns the branch/PR/CI/merge policy |
| `branch` | branch fixed at admission |
| `state` | delivery lifecycle state from §5.2 |
| `state_version` | monotonic compare-and-swap/fencing version |
| `state_entered_at` / `state_due_at` | per-state progress and escalation clock |
| `fact_progress_at` | newest authoritative artifact/session fact advance |
| `hold_kind` / `hold_reason` / `return_state` | total paused/needs-human overlay and legal resume target |
| `review_required` | true from admission unless an explicit future policy says otherwise |
| `builder_model_family` | actual family bound at launch, copied from the admitted seat |
| `builder_affinity_state` | `pending|active|parked|relaunching|cleaning|closed|abandoned`; does not replace `epics.state` |
| `artifact_version` | increments on a new current head |
| `review_round` | increments when a new independent judgment is required |
| `review_job_id` | unique link to the reusable review lease/execution row |
| `reviewer_identity` / `reviewer_model_family` | current live reviewer binding, empty otherwise |
| `verdict` / `verdict_head_sha` / `verdict_base_sha` | SHA-bound current verdict projection |
| `review_eligible_at` | resettable projection for the current head; not the backstop's sole clock |
| `dispatch_due_at` | durable stall/recovery deadline |
| `dispatch_attempted_at` / `review_started_at` / `reviewed_at` | lifecycle clocks |
| `recovery_count` / `last_recovered_at` | restart-stable recovery observability |
| `alert_pending` / `alerted_at` | closes the dispatch-success/alert-loss crash window |
| `last_error` | bounded operator-readable failure detail |
| `created_at` / `updated_at` | audit and ETag inputs |

The row and admission-key binding are created inside `Store.AddEpicRun`'s serialized
admission transaction.
`DeleteEpicRun`, which is only valid for a prelaunch rollback, removes it by FK
cascade. After launch, abandon is a state transition; history is never hard-deleted.

### 6.2 `epic_artifacts`

This table separates observed repository facts from desired pipeline state. In the
one-delivery implementation it is also 1:1 with the epic.

Recommended fields:

- `epic_id`, `project_id`, `repo`, `branch`;
- nullable `pr_number` and immutable `pr_bound_at`;
- `head_sha`, `base_sha`, `head_updated_at`, `artifact_version`;
- `is_draft`, `pr_open`, `closed_unmerged`;
- `ci_state` (`unknown|none|pending|green|red|infra_red`), required-check evidence,
  `check_contexts_truncated`, and `ci_has_real_success`;
- `ci_green_observed_at`, which is set from the authoritative green fact for the
  current SHAs and cleared on head/base advance; this—not a possibly-missed delivery
  projection—is the review-dispatch backstop clock;
- `mergeable_state`, `merged`, `merge_commit_sha`;
- `source_observed_at`, `source_updated_at`, and a monotonic watermark;
- bounded check failure names/URLs and artifact evidence references.

Only reconcile-in code writes observed fields. Agent result handlers may report that
they pushed or opened something, but that report is a nudge to refetch, not an
authoritative artifact fact.

`ci_state=green` has one exact meaning:

```text
CIRollup == SUCCESS
AND CIHasRealSuccess
AND every repository-required check is present and passed
AND NOT CheckContextsTruncated
```

`CINone`, pending, missing required checks, and truncated contexts are non-green and
arm the `awaiting_ci` clock. A red result requires a durable builder-rework action;
`infra_red` receives bounded fact refresh/retry before durable escalation through the
existing CI incident attention route.

### 6.3 Review execution link

The existing job/lease machinery should be reused for the execution semantics, not
for ownership discovery. Add an explicit `epic_run_id`/`epic_id` link to a native
review job and use a deterministic review job ID. Do not call the public
`AdoptPRForReview` domain path and do not stamp the job `adopted=1`.

Materialization occurs only when the delivery is review-eligible. Native epic review
jobs carry `workflow_domain='epic_v2'` (or an equivalent explicit epic-delivery link)
and are fenced out of legacy `ReconcileStuck`, `JanitorUnblock`, and generic job
liveness transitions. The delivery/review reconciler alone owns their capacity-wait,
hold, re-arm, and escalation semantics. A queued native review with no eligible
family stays `review_queued` with a capacity attention; legacy code must never move
its job to `needs_human` behind the delivery projection.

Under `epic_review_handoff_v2`, the generic `AdoptSweep` excludes every PR whose
`(repo, head branch)` is owned by an epic delivery. Epic builder/goal-session
contracts stop applying `needs-claude` or `flowbee:adopt`. Materialization also checks
for a pre-existing adopted job on the same `(repo, pr_number)`: it transactionally
absorbs/supersedes that job into the single native execution and fences its pending
outbox work rather than inserting a second review.

The materialization transaction must:

1. compare the delivery `state_version`, artifact version, and SHAs;
2. create or re-arm the single native epic review job;
3. copy `builder_model_family` for hard anti-affinity;
4. bind repo, PR, head, base, and authoritative diff/evidence;
5. set reviewer capabilities;
6. arm the existing no-eligible-independent-reviewer timer;
7. set `review_job_id`, move the delivery to `review_queued`, and enqueue/wake the
   Tmux-backed review action;
8. commit all of the above or none of it.

Review rejection and SHA movement need epic-specific transitions: they route a
durable rework action back to the existing builder session and re-arm review on the
new head. They must not accidentally enter the generic one-off issue build lane.

### 6.4 `epic_actions`

This is the transactional outbox for terminal and external actuation.

Recommended fields:

- `id`, `project_id`, `epic_id`, `kind`, `target_role`, `target_session_id`;
- `state`, `action_epoch`, `dedup_key`, `payload_ref`, `payload_hash`;
- `artifact_version`, `head_sha`, `review_round` where applicable;
- `attempts`, `next_attempt_at`, `delivery_started_at`, `acknowledged_at`;
- `review_started_at`, `last_reviewer_fact_at`, and verdict-progress deadline where
  the action is a review;
- `driver_receipt_json`, `last_error`, `created_at`, `updated_at`.

A full unique index covers every live/effect-history key; only
`cancelled_superseded` rows are excluded so a SHA revisit can re-derive an action.
This is not a general active-only index: acknowledged and dead-lettered keys remain
unique, and retries re-arm the same row. Payloads that can be large or sensitive should be immutable artifacts
referenced by hash, not copied into every row.

### 6.5 `projects` and `project_repos`

Phase 2 adds:

`projects`:

- stable `id` and human-readable slug/name;
- state (`active|paused|archived`);
- Interactor and Orchestrator route/session identities;
- scheduling `weight`, concurrency caps, and policy profile;
- default delivery repository;
- per-project breaker state/reason;
- created/updated timestamps.

`project_repos`:

- `(project_id, repo_id)` primary key;
- role (`delivery|context|dependency`), with one default delivery repo;
- optional project-specific policy overlay that may only tighten repo policy;
- enabled state and timestamps.

Every current durable object that can be routed or displayed gains `project_id`,
including work intents, epics, deliveries, artifacts, actions, attention, decisions,
WIP markers, audit, costs, and public digest rows. A temporary default project
backfills all existing records before `project_id` becomes required.

### 6.6 `decision_requests` and `decision_responses`

`decision_requests` is the dashboard/Interactor inbox:

- `id`, `project_id`, optional `epic_id` and `delivery_id`;
- closed `kind`: `question|plan_review|design_review|authorization|exception`;
- title, bounded prompt, structured options/schema, priority, due/defer clocks;
- `requested_by`, `route_to`, and expected response type;
- immutable subject artifact references, version, and cryptographic hash;
- evidence/context references and a bounded human-readable summary;
- state, request version, supersession link, timestamps.

`decision_responses` is append-only:

- request ID/version and subject artifact hash;
- response kind: `answer|approve|request_changes|defer|deny`;
- structured value plus optional bounded comment;
- authenticated actor, authorization scope, and timestamp;
- downstream acknowledgement status and audit link.

The current response is a projection over the append-only records. A response is
accepted only if the request is still current and its artifact hash matches.

### 6.7 `work_intents`

`work_intents` is the durable bridge from human/Interactor product conversation to
Orchestrator epic preparation. It is planning state, not yet a Flowbee-owned delivery.

Recommended fields:

- `id`, `project_id`, source conversation/message IDs, and creating Interactor
  incarnation;
- bounded title/summary plus immutable intent artifact reference, version, and hash;
- acceptance/definition evidence and linked required decision IDs;
- state from §5.6, state version, priority, and dependency references;
- paired Orchestrator route, delivery action ID, lease/route epoch, attempts, and
  acknowledgement clocks;
- current epic-contract artifact/hash while orchestrating;
- submission idempotency key and nullable admitted `epic_id`;
- hold reason, next retry, supersession/cancellation link, and audit timestamps.

At most one active work-intent version may exist for a source request. Promotion to
`ready_for_orchestrator` and insertion of its delivery action commit together. Epic
submission is idempotent on `(project_id, work_intent_id, intent_version)`, so an
uncertain acknowledgement can be verified or replayed without admitting two epics.

The dashboard may let the human pause, cancel, reprioritize, or answer a blocking
decision. It deliberately has no required "send to Flowbee" button: once definition
and policy gates are satisfied, promotion is automatic.

### 6.8 `control_events` ledger and digest sequence

The ledger is concrete, not conceptual:

```text
control_events(
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  epic_id TEXT,
  epic_seq INTEGER,
  kind TEXT NOT NULL,
  from_state TEXT,
  to_state TEXT,
  state_version INTEGER,
  actor_kind TEXT NOT NULL,
  actor_id TEXT,
  payload_json TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(epic_id, epic_seq)
)
```

`epic_deliveries` remains the compare-and-swap mutable authority used by scheduling;
it is **not** reconstructed by folding the ledger on every read. The ledger is the
append-only audit/change sequence. Every projection mutation appends its event in the
same transaction, including deletes/tombstones and delivery/action-only changes.
`epic_seq` is allocated from the current delivery/event stream inside that
transaction; global `seq` drives ETag/SSE digesting.

Until every legacy writer is retrofitted, the public digest is a hybrid maximum of
`control_events.seq` and legacy input cursors. The retrofit inventory is explicit:
supervisor/master attention leases, seat/session occupancy, WIP-marker changes,
capacity/account-window generations, attention records, and dashboard summary/KPI
projections, and epic/epic-delivery changes each append a `control_event` in the same
transaction. Only after all seven writer families emit events does the digest switch
to `MAX(control_events.seq)` alone;
the cutover is itself audited. At cutover, insert a seed event with an explicit
`seq` greater than or equal to the maximum legacy Unix-millisecond digest and current
Unix milliseconds. SQLite then continues AUTOINCREMENT above that seed. A pre-migration
ETag can therefore never outrank a post-migration event.

Every state change appends an event in the same transaction as its projection write.
At minimum add event kinds for:

- epic admitted and delivery created;
- artifact bound / artifact advanced;
- CI eligibility established;
- review dispatch required / materialized / recovered / claimed;
- review approved / changes requested / superseded;
- builder rework sent and acknowledged;
- merge queued / started / conflicted / merged;
- cleanup queued / completed;
- decision requested / viewed / responded / superseded;
- work intent captured / gated / ready / delivered / acknowledged / admitted;
- project breaker opened / closed;
- capacity observation accepted / rejected and seat route committed.

The public digest sequence advances when any linked delivery, action, decision,
attention, project, capacity, or alert fact changes. SSE remains a lossy wake-up hint;
ETag and read APIs return truth.

### 6.9 `conversation_threads` and `conversation_messages`

Phase 1 also persists the focused human↔Interactor conversation shown in each project
workspace.

`conversation_threads` carries stable thread ID, project ID, Interactor route,
optional epic/decision focus, state, title, last message sequence, and timestamps.
`conversation_messages` is append-only and carries thread/message sequence, role,
authenticated actor/agent incarnation, content artifact/hash, delivery/stream state,
reply-to ID, and timestamps.

Conversation may frame work, explore tradeoffs, and lead to a typed request. It is not
hidden workflow state. An actionable approval, authorization, plan verdict, or answer
must be promoted into the typed decision model and explicitly confirmed there. A
transcript sentence such as "looks good" cannot advance a gate by itself.

## 7. Terminal observation and actuation contracts

### 7.1 Authoritative remote target and optional attach proxy

The durable target is the agent session on the remote tmux server where the seat
lives. It carries:

```text
project_id, epic_id, role, seat_id
host == seats.box, ssh_user, remote_tmux_server
remote_session, remote_pane, pane_instance_id, session_incarnation
optional local_attach_session/local_attach_pane
```

The remote `tmux has-session` result, obtained through the seat's normal SSH channel,
is the authoritative session-existence fact. A local attach proxy is lossy transport
and never proves launch, processing, or death. Launch/rework acknowledgement requires
the remote session fact plus the agent's contract/assignment acknowledgement.
Cleanup targets the remote session and any local attach proxy independently and does
not release physical occupancy until remote absence is observed. `target.host` and
capacity identity use the same `seats.box`; no second host vocabulary is allowed.

A failed SSH/tmux channel is `seat_channel_unreachable`, not evidence that the remote
session is absent. Its recovery re-establishes the channel and refetches the remote
fact without consuming another send attempt.

### 7.2 DriverPort observation, control, and actuation

The DriverPort exposes closed, typed operations in three capability groups:

Observation:

- `HasRemoteSession(target)`;
- `Capture(target)`;
- `Classify(capture)`;
- `Address(target) → {tmux_target, pane_instance_id, cursor}`.

Control/actuation:

- `EnsureSession`, `Stop`, and `Reattach` against exact stable identities;
- lease acquire/release, draft stash/restore, verified receipt, and control-ledger APIs;
- bounded text/key insertion and approved permission/dialog/credential handling through
  the embedded routed-input engine, never raw calls from Flowbee.

The DriverPort implementation never reflects arbitrary captured text into a command;
all inputs are closed-template, grant-scoped, pane-locked, and auditable. Pane content
remains untrusted evidence. Flowbee alone interprets observations against workflow
state/version/epoch fences.

### 7.3 DriverPort delivery and receipt protocol

Every actuation begins with a durable action. One executor atomically claims
`pending → delivering` with a new epoch and passes the immutable payload to
the Driver's embedded verified-input engine with `expect_pane_instance`. A pane-instance mismatch fences the action
before input. The actuator returns its protocol status plus action ID, payload hash,
target/session incarnation, and bounded diagnostics:

| status | durable interpretation and recovery |
| ---: | --- |
| `0` | submitted and transport-verified; never resend the payload; wait for stage acknowledgement |
| `1–2` | invocation/tool/config failure before a trustworthy verified submission; hold and repair target/config before retry |
| `3` | target pane not found; refetch remote `has-session`, then relaunch/re-home or mark the seat channel/session missing |
| `4` | text may be present but submission is unverified; do **not** resend the payload; observe and use the same action's nudge path |
| `5` | interactive dialog; create a typed dialog/operational hold and use only an authorized closed-template answer action |
| `6` | unsafe existing draft; leave it intact and retry the same action after the draft hold clears; never force |

The receipt is committed before the stage projection advances. A crash with an
uncertain outcome enters `verifying`; recovery checks the remote session,
`pane_instance_id`, action/payload evidence, and agent acknowledgement before any
retry. The same action ID is never intentionally sent twice, and status `4` can never
cause a full-payload resend.

Launch, stop, lease, draft, and control operations are Driver methods behind the same
action epoch and remote-fact verification discipline. Flowbee never calls raw tmux or
standalone `tmux-send`; it calls DriverPort.

### 7.4 Stage acknowledgement

Submission is not completion. Required acknowledgements differ by action:

- launch build/review: remote session exists with the expected incarnation and the
  agent acknowledges the exact contract/assignment;
- builder rework: fenced relaunch is acknowledged, then status/head progress is
  observed;
- operational steer: prompt accepted, then the requested durable fact changes;
- stop/cleanup: authoritative remote session, optional proxy, worktree, and branch
  absence are each observed;
- merge: reconciled GitHub merge commit, never terminal text.

An action submitted but not processed by its acknowledgement deadline reopens with a
distinct `not_processed` reason and bounded recovery count. Exhaustion dead-letters
the action and moves the delivery to its registered visible hold; it never leaves a
submitted action as an unowned terminal state.

### 7.5 Flowbee-session death recovery

If the Flowbee control-plane session dies, the Driver does not infer completion from
pane silence and does not restart or recover agents as an autonomous product action.
On restart, Flowbee reacquires the canonical writer lock, fences stale action/lease
epochs, reads durable desired state, verifies each remote `tmux has-session` fact,
and reconstructs launch, wake, lease, draft, receipt, and cleanup work from the
outbox. The Driver then resumes or verifies the existing action under its original
idempotency key. `restart/recover-agents` remains outside the Driver boundary for
now; the control plane may surface a typed `seat_channel_unreachable`,
`builder_host_unreachable`, or `agent_session_missing` hold to the operational layer
instead of guessing or blindly relaunching.

### 7.5 Trust and safety boundary

- Pane content, PR text, issue text, logs, and artifact prose are untrusted data.
- Control verbs are closed templates. No captured text is reflected into keystrokes.
- Agent guidance payloads are authenticated, length-limited, control-byte rejected,
  hashed, and bracketed-pasted by `tmux-send`.
- DriverPort has no GitHub credential or Flowbee workflow-transition authority; it
  may mutate terminal state only under a Flowbee action, grant, lease, and epoch.
- A transport receipt proves transport behavior only; the deterministic plane decides
  whether the corresponding invariant is satisfied.
- Manual terminal activity remains a debugging fallback and must be reconciled and
  audited before Flowbee proceeds.

### 7.6 Driver contract fixtures and known gaps

Flowbee's `internal/driver` package models the binding v2.2 surface behind
`DriverPort`; the fake is the only transport used until the shipped Driver client
passes conformance on macOS and Linux. Fixtures cover same-CWD sessions, pane reuse,
cursor gaps/store resets, directional grant denial, crash-uncertain delivery, and
idempotent replay. The following wire details are intentionally recorded as contract
gaps rather than invented: canonical discovery/session snapshot envelopes, grant
create/get/delete bodies, receipt-by-action response shape, Ensure/Stop/Reattach launch
specs, stream frame schemas, cursor MAC encoding, and SDK/UDS transport details.
Each gap blocks only the real adapter, not Flowbee domain tests.

## 8. Reconciliation and self-healing loops

The main rule is simple: reconcilers compare durable desired state with reconciled
facts and idempotently ensure the next action. They do not reconstruct ownership from
the outside world.

### 8.1 Epic artifact reconciler

On startup and on the normal GitHub sweep cadence:

1. list admitted, non-terminal epic deliveries by repository;
2. fetch PR facts including `headRefName`;
3. join only artifacts proven to belong to existing epic deliveries;
4. reject fork heads, repo mismatch, branch near-misses, and ambiguous duplicate PRs;
5. monotonically persist PR/head/base/draft/CI/merge facts;
6. compute the legal delivery transition;
7. persist the required next-action intent in the same transaction;
8. publish a lossy SSE nudge after commit.

The GitHub `PullRequest` read model must carry the head ref name. Until that sweep
change lands, the Phase-0 baseline is the already-wired `EpicForHeadSHA` association:
per-epic mirror tip reads, exact SHA matching, and fail-closed behavior on mirror I/O
error. Only after `headRefName` is present and tested does exact
`(delivery_repo, branch)` become the primary fleet-wide lookup; SHA remains the
content/merge fence. `EpicForRepoBranch` must not be treated as wired before that
read-model PR lands.

No label is required. No generic PR adoption method is called. Under the v2 handoff
flag, `AdoptSweep` first excludes PRs owned by an epic branch; actor contracts forbid
epic sessions from applying adoption labels. An unrelated PR is a no-op even if it is
green, requested for review, authored by a known seat, or closes an issue referenced
by an epic. Until `headRefName` is wired, the exclusion falls back to exact
`EpicForHeadSHA` mirror-tip matching; a `(repo, pr_number)` absorb/supersede check
remains the final duplicate-job backstop.

### 8.2 Primary review handoff

When an owned artifact first becomes review-eligible, the fact transaction:

- records artifact `ci_green_observed_at`, resets current-head
  `review_eligible_at`, and derives `dispatch_due_at`;
- transitions to `awaiting_review_dispatch`;
- commits those facts against the admission-created review obligation.

In the same reconciliation pass, a second idempotent transaction materializes or
re-arms the unique native epic review job and advances to `review_queued`. The job is
the durable dispatch obligation; a `tmux-send`/wake action is created only as part of
the atomic reviewer claim, never as proof that a reviewer is already assigned. A
claim reconciler scans `review_queued` even when eligible reviewers are idle and
atomically binds one reviewer lease before creating that actuator action. This
intentional durable seam makes the production crash recoverable and visible: if the
process dies between the transactions, the database still says
`awaiting_review_dispatch` and the admission-time obligation plus artifact facts imply
exactly what must happen next. If it dies after materialization, the job remains
claimable. At no point is an agent transcript the only copy of "review this."

### 8.3 Review-dispatch stall reconciler

The immediate transaction is the normal path. A periodic backstop handles partial
legacy state, unexpected projection bugs, or a committed `awaiting_review_dispatch`
row whose materialization failed.

The stale predicate is deliberately fact- and obligation-based, not dependent on a
possibly-corrupt delivery-state projection:

```text
delivery.review_required
AND artifact is owned by this epic/repo/branch
AND artifact.pr_open
AND NOT artifact.is_draft
AND artifact.ci_state == green as defined in §6.2
AND artifact head/base are current
AND current verdict for head/base is absent
AND no current pending/delivering/queued/leased review action or execution exists
AND artifact.ci_green_observed_at <= now - review_handoff_stall
AND delivery is non-terminal and not abandoned
AND delivery has no paused/needs_human hold
```

For every match, `EnsureEpicReviewDispatched` atomically:

1. rechecks the predicate and state version;
2. normalizes any drifted delivery projection to `awaiting_review_dispatch`;
3. creates or repairs the deterministic review job/action;
4. increments `recovery_count`;
5. persists `alert_pending` and recovery evidence;
6. upserts one `review_dispatch_stalled` attention item, or leaves a durable pending
   bit for the alert drainer;
7. publishes a recovery event after commit.

Repeated or concurrent sweeps are harmless. An already-queued review with no reviewer
lease is a **capacity wait**, not a missing handoff; it is handled by the independent
no-eligible-reviewer timer/attention and is never redispatched. An active review lease
or current pending/delivering action is not redispatched. A verdict for the current
SHA is not redispatched. A stale lease is reaped through the normal epoch path before
the review is eligible again.

The alert remains visible at least until a reviewer actually claims the work. Its
dedup key includes epic and head SHA, so one head creates one active alert while a
later regression can alert independently. Recovery is recorded even if the condition
clears quickly.

Automatic recovery is capped per `(epic_id, head_sha, recovery_code)`. After the
configured maximum, or when the same derived action has dead-lettered, the backstop
must not recreate it forever: it applies a `needs_human` hold, raises
`review_dispatch_flapping`, and increments a thresholded
`flowbee_review_handoff_recoveries_total` series. A later head receives a fresh budget.

### 8.4 Review lease and reviewer death

The existing atomic `review_pending → code_review` claim is retained. The claim:

- binds identity, model family, lens, lease ID, deadline, and epoch;
- excludes the builder identity and builder model family;
- excludes an identity that already supplied an approval in the current round;
- loses cleanly if another reviewer wins first.

Heartbeats renew the live lease. Expiry/revocation returns the native epic review job
to `review_pending`, clears the delivery's current reviewer projection, increments the
epoch, and republishes availability. A delayed reviewer result carrying the old epoch
is rejected.

The lease is not proof of reviewer progress. `review_started_at` and
`last_reviewer_fact_at` drive a separate `review_verdict_stall` clock; a live lease
with no verdict/evidence progress becomes `review_verdict_overdue`, then is fenced and
requeued after the bounded recovery window. A renewing but hung reviewer therefore
cannot occupy `in_review` indefinitely.

No opposite-family capacity is a distinct visible condition. It fires the existing
no-eligible-worker mechanism plus epic attention; it never relaxes anti-affinity.
Native `workflow_domain='epic_v2'` jobs are excluded from legacy stuck, janitor, and
generic liveness state changes, but `ReviewPendingCandidates` remains eligible for
the v2 claim/reconcile path and must never be fenced. A consistency reconciler asserts that the linked job
and delivery axes agree before either can dispatch or merge; drift is repaired or
held, never left as `delivery=review_queued` with `job=needs_human`.

### 8.5 Rejection and rebuild reconciler

A `request_changes` verdict commits three things together:

- immutable review evidence bound to the rejected head;
- delivery transition to `changes_requested` with incremented review round;
- one durable `builder_rework` action addressed to the epic's parked builder
  affinity, containing the bounded findings/artifact reference.

The action executor performs a capacity-gated fenced relaunch on the authoritative
remote seat, then uses `tmux-send` and waits for acknowledgement. There is no live
parked pane to steer.
When reconcile-in sees a new branch head, it invalidates prior eligibility, returns to
`awaiting_ci`, and eventually dispatches a new independent review. It never creates a second
delivery branch to avoid dealing with the first rejection.

### 8.6 Merge, conflict, and cleanup reconcilers

Approval for the exact current head/base creates one merge action. The merge gate
rechecks:

- current SHA-bound independent verdict;
- current required CI at head;
- content/scope/evidence gates;
- repository merge policy and base freshness;
- no project/repository circuit breaker;
- no newer artifact version;
- delivery is non-terminal, not abandoned, and has no paused/needs-human hold.

Any head/base advance cancels the old-head review/merge action and returns the
delivery to `awaiting_ci`; the executor does not retry a superseded merge. An
unresolvable conflict becomes a `needs_human` hold with an explicit return or abandon
edge, not an unclassified retry loop.

An ambiguous or genuine merge conflict transitions to `conflict_resolution` and
creates fenced resolution work. A resolved head always re-runs CI and independent review; a
conflict resolver cannot approve its own resolution.

After GitHub reports a concrete merge commit, cleanup becomes its own durable action:
stop obsolete sessions, remove only the epic's registered worktree, delete only the
registered merged branch, release residual reservations, finalize artifact/history
records, and run configured post-merge verification. Each target is explicit and
identity-checked; no broad glob or inferred path is used.

### 8.7 Decision and notification reconcilers

Decision requests and responses also use outbox semantics:

- Flowbee/Orchestrator creates the typed request and notification action together;
- the correct project Interactor is notified through its durable route;
- dashboard/SSE notification can be retried independently;
- the human response is committed before downstream delivery;
- Interactor, Orchestrator, and Flowbee acknowledge consumption in order;
- a missing acknowledgement is retried or surfaced, never treated as consent.

The escalation chain is:

```text
Flowbee operational condition
  → project Orchestrator when product/logistics judgment is needed
  → project Interactor when human conversation or authority is needed
  → human through the global Needs You inbox
```

Operationally recoverable conditions remain in Flowbee. The system must not page the
human merely because a transient delivery was retried successfully.

The current single `masterID` maps explicitly during migration:

| existing mechanism | v2 owner |
| --- | --- |
| `LeaseAttention(masterID)` for operational pipeline recovery | global operational Grok actor with capability `flowbee.operations` |
| `KindMasterAbsent` | `operational_supervisor_absent`; global in Phase 0, later scoped to the operational pool |
| product/logistics judgment | project Orchestrator through a project-scoped escalation route, never the operational `masterID` |
| human authority | project Interactor → global Needs You inbox |

Orchestrator absence is a separate per-project `orchestrator_route_absent` condition.
An operational Grok actor cannot consume product attention merely because it leases
the legacy master slot.

### 8.8 Work-intent promotion reconciler

The routine product path is automatic:

```text
human request
  → Interactor captures/version-binds work intent
  → missing definition or required approval becomes a typed decision
  → satisfied intent is delivered to the paired Orchestrator
  → Orchestrator prepares and idempotently submits one epic
  → Flowbee admission links the epic and acknowledges the route
```

Neither the dashboard nor the human performs a separate plumbing transition. The
Interactor's standing role contract requires promotion as soon as definition and
policy gates are satisfied. The paired Orchestrator's standing contract requires it
to claim ready intents and submit complete epics without waiting for a reminder.

The reconciler scans every non-terminal work intent on startup and periodically. It:

1. advances `awaiting_decision` when every linked current decision is resolved and
   its subject hashes still match;
2. ensures one durable Orchestrator-delivery action for each
   `ready_for_orchestrator` intent;
3. reclaims expired route leases and fences stale Interactor/Orchestrator
   incarnations;
4. verifies an uncertain submission by its idempotency key before replaying it;
5. links an already-admitted epic instead of submitting a duplicate;
6. creates one `work_intent_promotion_stalled` attention item when any eligible hop
   exceeds its acknowledgement deadline;
7. routes genuine missing product definition back as a typed question, never as a
   generic "should I send this?" prompt.

A human can explicitly pause, cancel, reprioritize, or change an intent. Absent such
an instruction, successful definition/approval is the go-signal and the actors keep
the route moving.

### 8.9 Delivery-wide progress backstop

The review-dispatch reconciler is one specialization of a delivery-agnostic backstop.
Every non-terminal, non-paused, non-`needs_human` delivery is scanned against its
registered `state_due_at`, `fact_progress_at`, current action/lease, and authoritative
facts. The registry is executable data shared with §18 and the property tests:

| state/fact | automatic next action | durable attention after due |
| --- | --- | --- |
| `admitted`, no launch acknowledgement | idempotently ensure builder-launch action | `builder_launch_stalled` |
| `building`, no status/artifact fact progress | observe remote session; bounded operational recovery | `builder_progress_stalled` |
| `awaiting_artifact` | refetch exact branch/PR and nudge or relaunch builder | `artifact_never_produced` |
| `awaiting_ci` + `unknown|none|pending` | refetch checks; keep visible wait | `ci_never_green` |
| `awaiting_ci` + `red` | ensure one SHA-keyed builder-rework action | existing `KindCIRedOnEpicPR` |
| `awaiting_ci` + `infra_red` | bounded refetch/retry and breaker probe | existing `KindCIInfraIncident` |
| `awaiting_review_dispatch` | §8.3 ensure review | `review_dispatch_stalled` |
| `review_queued`, no eligible family | retain one job; retry on capacity change | `review_capacity_unavailable` |
| `review_queued`, eligible family but no claim | atomically claim one reviewer and create its target-specific Driver action | `review_claim_stalled` |
| `in_review`, stale lease | fence/reap/requeue | `review_lease_expired` |
| `in_review`, live lease but no reviewer fact/verdict progress | fence after bounded verdict window and requeue the same review round | `review_verdict_overdue` |
| `changes_requested|rebuild_in_flight` | ensure relaunch/rework or await new head | `rework_dispatch_stalled` |
| `conflict_resolution` with no new head/base fact | observe resolver result; bounded retry, then retain a resumable hold | `conflict_resolution_stalled` |
| `merge_queued|merging` | verify/retry exact-head merge effect | `merge_dispatch_stalled` / `merge_outcome_uncertain` |
| `merged` | atomically enqueue/claim explicit cleanup action, or verify the cleanup fact | `cleanup_overdue` |
| `cleanup_pending` | verify/retry explicit cleanup targets | `cleanup_overdue` |

Dead-lettered rework, merge, and cleanup effects use the same bounded recovery cap as
review dispatch. A transient dead letter is re-armed with an `UPDATE` of the same
historical action (`dead_letter → pending`); it never inserts a replacement key.
Human clear explicitly grants a fresh recovery budget for `(epic_id, head_sha,
recovery_code)`. The contract compiler fails if any delivery state, CI substate, or
effect seam lacks one registry row.

### 8.10 Independent alert drainer and reconciler supervision

`alert_pending` is drained by its own reconciler and ticker from one durable alert
outbox, not only from delivery rows. Capacity-pool exhaustion, reconciler death,
action dead letters, route failures, attention leases, and delivery stalls all insert
the same project-scoped alert record with a dedup key, payload, attempt/lease epoch,
and `alert_pending` bit in their source transaction. A condition that self-heals
before the next stall pass still emits the one committed alert/audit event.

Every reconciler uses one shared supervised-loop wrapper with per-iteration panic
recovery, bounded item isolation, and a durable heartbeat recording last start,
success, error, panic, cursor, and ledger sequence. An in-process watchdog raises a
durable `reconciler_dead` attention when a heartbeat ages past the configured number
of intervals. A poison row is quarantined/held and cannot crashloop readiness for the
whole plane.
An independent hold-overdue pass scans expired `state_due_at`/`defer_until` values
without a registered action and emits `hold_overdue` attention. The external
dead-man's-switch is the maximum-stall SLO for a hung leader; an optional voluntary
exit self-check may terminate a process that cannot advance its heartbeat, but no
automatic failover assumes overlapping SQLite writers.

Phase 0 also ships:

- a durable alert projector which converts every human-facing alert into an immutable
  `system` message for the exact current project Interactor, then uses the normal
  Driver grant/action/receipt/evidence path. There is no Matrix, provider-specific
  sink, or alternate terminal transport. Driver submission is not acknowledgement;
  the source alert remains outstanding until exact Interactor processing evidence is
  committed. If the Interactor route is unavailable, the alert remains visibly held
  and retryable; and
- `flowbee watchdog`, run by cron/service independently of the Flowbee writer, which
  polls the last completed reconcile pass and digest advance. It durably retains each
  project-bound incident and submits it to Flowbee's signed control-alert ingress;
  after acceptance, the same Interactor projection owns delivery. A Flowbee process
  outage therefore cannot lose the incident, although Interactor delivery waits for
  the ingress to become reachable again.

Active v2 mode fails readiness if the exact project Interactor notification route or
the configured external watchdog identity/project binding is absent. It never falls
back to a generic webhook destination or another project's Interactor.

### 8.11 Builder launch and audited re-home

A builder-launch reconciler covers `admitted → building`: it ensures one fenced
launch action when capacity permits, verifies the remote session/contract ack, and
applies a visible hold after bounded attempts rather than abandoning the epic.

If a parked epic's host/seat is durably unreachable, a `rehome_delivery` domain
transition may fence the old incarnation, verify the branch exists on GitHub and no
unobserved local work can be authoritative, recreate the worktree on a compatible
seat, bind a new builder identity/family, and relaunch. The transition is audited and
forces CI plus independent re-review. If unpushed work cannot be ruled out, it
requires a typed human decision; break-glass tmux intervention is not the normal
path.

## 9. Cross-cutting usage and capacity safety

This workstream is a Phase-0 safety foundation, developed and gated in parallel with
the incident slice. It is not a prerequisite for shipping the durable review-handoff
slice in §10, but v2 fail-closed routing cannot activate until its own collector,
alert, and fleet-onboarding exit gates pass.

### 9.1 Primary-source model to adapt

The architecture is informed by the
[domanski-ai/headroom primary repository at audited commit
`3002c659`](https://github.com/domanski-ai/headroom/tree/3002c659a0345bf015dc6e193515ac9d79966a7d),
not by screen scraping or second-hand descriptions. The useful properties to adapt
are:

- live, read-only provider data rather than spending tokens to probe;
- Codex rate-limit reads through the app-server, whose official surface includes
  `account/read`, `account/rateLimits/read`, and rate-limit update notifications
  ([Codex app-server reference](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md));
- account identity bound to the usage result, so a swapped login cannot inherit a
  prior account's headroom;
- fail-closed routing for stale, unverifiable, malformed, cooldown, or unavailable
  observations;
- explicit reserve thresholds across every applicable window;
- atomic snapshot publication;
- append-only sampled history for trends and cap-hit episodes;
- per-account launch/slot locking so concurrent routing decisions cannot choose the
  same scarce login.

Headroom supports Claude and Codex, not Grok. Its architecture can be adapted, but
Flowbee's Grok billing endpoint, authentication, schema, throttling, and identity
binding must be independently verified and contract-tested. A similarly named field
is not sufficient evidence of equivalent semantics.

Audited source anchors at that commit are the
[Codex app-server collection and identity path](https://github.com/domanski-ai/headroom/blob/3002c659a0345bf015dc6e193515ac9d79966a7d/headroom/collect.py#L434-L680),
[routing-time identity/freshness gate](https://github.com/domanski-ai/headroom/blob/3002c659a0345bf015dc6e193515ac9d79966a7d/headroom/route.py#L411-L637),
[atomic persistence helper](https://github.com/domanski-ai/headroom/blob/3002c659a0345bf015dc6e193515ac9d79966a7d/headroom/paths.py#L122-L155),
and [bounded history implementation](https://github.com/domanski-ai/headroom/blob/3002c659a0345bf015dc6e193515ac9d79966a7d/headroom/history.py#L314-L390).

### 9.2 Current Flowbee gap

Flowbee already contains more live-provider work than the older epic-lane plan
assumes:

- `acctprobe.ProbeCodexLive` uses the Codex app-server;
- `acctprobe.ProbeGrokLive` uses a Grok billing adapter;
- local seat probing can call the tiered live probes;
- `acctprobe.MarkIntegrity` can detect duplicate identities;
- results carry trust state, reset windows, `RetryAt`, and
  `LiveUnavailableReason`.

The durability and routing wiring is incomplete:

- remote Codex seats use cache-only `ProbeCodexHome`, which is display-only, yet
  `classifySeatHealth` can mark the result ready;
- the selector does not enforce the stored `AccountWindow.Routable` decision and
  explicitly fails open on stale or missing capacity;
- remote Grok can combine a newly read auth identity with an older, unstamped local
  usage-log entry and currently treat the composite as routable;
- seat re-login drift is not detected because `account_key` is only filled when it is
  empty rather than continuously checked against an expected lineage;
- the consolidated fold never calls `MarkIntegrity`; using it correctly requires
  coalescing the same account across hosts as one quota bucket, not blindly declaring
  every repeated account a duplicate;
- a duplicate config home on the same host is not structurally refused, while a
  legitimate same account on multiple hosts needs explicit shared-quota semantics;
- persisted `probe_stale` is evaluated at write time, so an old verified row can stay
  routable forever if the ticker dies;
- the fold writes one account-keyed projection seat by seat, so a shared account's
  final result depends on `ListSeats` order: a later remote display-only seat can
  demote an earlier live verified result, and weaker `verified_local` data can
  overwrite stronger live windows;
- `classifySeatHealth` checks criticality before trust, so stale-critical can appear
  `limit_critical` while display-only normal can appear `ready`; neither is a safe
  scheduling classification;
- seats are probed serially with no provider/host concurrency bounds, account
  single-flight, durable backoff, or timeout isolation; one wedged probe can delay the
  whole fold;
- persistence/UI drops `RetryAt`, source, hold reason, live-unavailable reason,
  credential/identity digests, and window duration;
- there is no append-only observation history;
- a routing decision is not coupled to a fresh recheck and durable seat/account
  lease;
- Grok's monthly window is currently mislabeled `weekly_all`, which makes reset and
  reserve policy semantically wrong;
- the Codex app-server client sends `initialized` and begins account reads immediately
  after the initialize request instead of waiting for the initialize response, which
  violates the protocol ordering and can race startup;
- scoped windows use an unstable/truncated descriptive codename rather than a stable
  upstream limit ID, so distinct limits can collide.

The result is that the dashboard may look informative while the scheduler is using a
weaker, stale, or duplicate identity fact.

### 9.3 Target identity model

Separate the account from the place it is logged in:

- **account**: provider-global identity and quota lineage;
- **seat**: one account login on one host/config home;
- **session lease**: a fenced, time-bounded allocation of that seat/account capacity
  to one build/review session.

Every account registration carries an **expected** provider account key,
credential/config-home digest, and lineage version. Every observation carries:

- provider and provider-global account key;
- verified identity lineage/fingerprint, excluding raw secrets;
- seat ID, host, config-home identity, and collector identity;
- source (`live_app_server|live_billing|cache|display_only`);
- trust state and integrity result;
- fetched-at, persisted-at, and provider/server clock where available;
- all usage windows with scope/model, used percentage, resets-at, and cap status;
- retry/backoff metadata and live-unavailable reason;
- schema/adapter version.

Two seats on different hosts can legitimately share one account quota bucket and are
coalesced globally for usage/reserve decisions. Two registered homes resolving to the
same canonical home on one host are rejected. Two nominally distinct account slots
that unexpectedly resolve to one provider identity are quarantined until corrected.
An identity or credential-lineage swap on a seat immediately fences the old lineage,
holds the seat, and requires explicit rebinding; it never overwrites history as if the
same account merely changed a label.

The operational Grok role is a first-class capacity consumer with its own seat lease
and minimum reserve. It may share an account-level quota bucket with reviewers, but
it cannot consume the configured review-pool reserve. Role capability and physical
occupancy are recorded separately; registering a Grok model does not make one pane
simultaneously available for operations and review.

### 9.4 Remote live collector

Remote probing must be live. Add a narrow authenticated collector RPC/reporting path
on each host rather than copying a cache file to the control plane. The collector:

1. resolves the registered config home without exporting credentials;
2. invokes the provider's live read locally;
3. for Codex, waits for the initialize response before sending `initialized` or any
   account/rate-limit reads;
4. binds the result to the live `account/read` identity and holds any expected/actual
   mismatch;
5. returns only the sanitized normalized observation;
6. includes the expected-vs-observed identity and credential-lineage evidence;
7. signs/authenticates the report as the registered host/seat collector;
8. never chooses a route or mutates scheduler state.

If the live read is unavailable, a cache/display-only result may still be stored and
shown, but it is never routable. "Visible" and "safe to schedule" are separate
properties.

### 9.5 Observation store and latest projection

Add an append-only `account_usage_observations` table and retain `account_windows` as
the latest trusted projection.

Observation fields include the normalized data in §9.3 plus raw-response hash,
validation outcome, rejection reason, and superseded lineage. `last_attempt_at` and
`last_good_at` are distinct clocks; a failed probe must never make an old measurement
look fresh. Secrets and raw tokens are never stored. A single transaction inserts the
observation, runs integrity and monotonic checks, and updates the latest projection
only if it is newer and trusted.

All rows from one fleet fold should publish as one atomic generation, or behind one
atomically advanced active-generation pointer. A window/projection failure must not
leave the corresponding seat marked ready against a partial generation.

Persistence must be atomic and interruption-safe. A crash after the provider read but
before commit simply causes another read. A crash after commit leaves a complete
observation/projection pair. History retention and sampling are configurable; the
default should preserve enough resolution to explain cap hits, reset behavior, and
scheduler decisions without retaining message or transcript content.

The fold gathers every seat result before updating an account projection. It groups by
`(provider, account_key)` and applies one deterministic reducer:

1. valid live/identity-verified sources outrank verified-local, cache, display-only,
   stale, and failed sources;
2. within equal source/trust rank, the newest provider capture wins;
3. a lower-trust normal reading cannot clear a higher-trust critical window;
4. failed seats contribute seat health/error evidence but cannot demote a healthy
   account projection;
5. the reducer persists the account projection once, independent of seat list order;
6. each seat's own health remains independent so one bad login is visible without
   poisoning another host's valid seat.

Once this gather/reduce generation is enabled, the legacy per-seat
`foldSeatCapacity → UpsertAccountLimits` writer is disabled under the same feature
flag and cannot mutate `account_windows`. There is one projection writer. Shadow mode
may compare its proposed output but must remain read-only.

### 9.6 Backoff, single-flight, and provider protection

- Provider-wide and host-wide concurrency are bounded so one slow provider/host cannot
  wedge the serial fleet fold.
- At most one live probe per provider account runs at a time, even if several seats
  share it.
- Provider and account-level backoff honors `RetryAt`/`Retry-After`, adds bounded
  jitter, and survives process restart in durable state.
- A provider-wide outage opens a probe circuit breaker; it does not trigger a tight
  loop across every seat.
- Successful probes gradually close the breaker.
- Probe failures preserve the last trusted measurement for display with an increasing
  age; they do not refresh its freshness clock.
- A cache fallback records why live data was unavailable and remains non-routable.
- Window kinds are provider-neutral but semantically exact. A Grok monthly window is
  `monthly`, never relabeled as weekly merely to fit an existing column.
- Scoped windows retain the provider's stable upstream limit identifier; a friendly
  codename is display metadata and never the uniqueness key.
- A reset timestamp passing does not imply recovered capacity. The old observation
  becomes non-routable until a fresh live probe proves the new window.
- Provider captures unreasonably in the future, reset times inconsistent with the
  window, or clock skew beyond policy are held rather than normalized into freshness.

Provider adapters publish an explicit required-window contract:

| provider | required/absent semantics |
| --- | --- |
| Codex | the live `codex` weekly window is required; absence is `required_window_missing` and held. A 300-minute/session window is gated when returned/declared applicable, but its explicit absence does not synthesize zero. Optional model-scoped windows may be omitted; every returned applicable window still gates. |
| Grok | a valid active billing period is required. `creditUsagePercent` absent **with** that period present is the provider-defined known `0%`, not unknown. No billing period/config, malformed period, or unknown schema is held. Weekly and monthly periods retain distinct kinds. |

An absent required window is visible and non-routable but is not mislabeled a
provider outage. An absent optional/scoped window is skipped. An unknown value never
becomes zero except for the explicit Grok contract above, which is pinned by captured
fixtures.

### 9.7 Routing gate and seat lease

Capacity routing becomes fail-closed by default:

```text
routable(now) = enabled seat
        AND seat health == ready
        AND this seat's own latest live observation is verified and fresh
        AND this seat's identity/credential lineage matches its registered expectation
        AND the coalesced account projection is verified and fresh
        AND every relevant window has headroom above reserve
        AND no cooldown/provider backoff applies
        AND seat/account concurrency lease can be atomically acquired
```

Freshness is computed from `now - fetched_at` on every routing read; a persisted stale
flag is display metadata, not the authority. If the collector/ticker dies, observations
age out of routability without any new write.

Builder, reviewer, and operational-agent selection all call this one predicate.
`capacity.SelectAccount`/`SelectAccountForModel` and
`worker_accounts.usage_pct` are retired for epic-v2 routing or hardened to delegate to
the same gate; no review path may retain a freshness-free selector.

Immediately before launch or parked-affinity relaunch, Flowbee performs or joins a
fresh-enough live recheck and atomically commits both the epic/session assignment and
the seat/account compute lease. A timeout/failure is a distinct durable held dispatch
with bounded retry and capacity attention; it is neither a silent no-op nor permission
to fall back to aged data.
Two concurrent launchers therefore cannot both consume the same last slot or quota
assumption.

When a builder parks for CI/review, its remote pane is torn down and the active
compute/account lease is released. The epic keeps worktree/branch/builder identity
affinity and scope reservation. Rework must reacquire safe compute capacity before a
new fenced session incarnation is launched. That relaunch explicitly transitions
legacy `epics.state` from `done|achieved` back to `launching` so
`epicActiveStatesSQL` counts the delivery again; the v2 builder-rework outbox is
fenced away from the legacy `masters.go` steer path.

Admission/dispatch is also family-safe: a build family is eligible only if at least
one configured **distinct** review family can serve it. The shipped default admits
Codex builders and Grok reviewers; Grok and Claude builder seats are disabled for new
v2 admissions unless a distinct review family is configured. Before canary, every
in-flight Grok/Claude-built epic is inventoried and either assigned a configured
distinct-family reviewer (for example Claude/Codex for a Grok build) or held for an
audited rebuild/abandon decision. Same-family review is never the migration fallback.

A `capacity_pool_exhausted` reconciler tracks each project and required
`build|review|operations` pool. If eligible queued work sees zero routable seats past
`capacity_pool_exhausted_s`, including zero caused by reserves or a failed live
collector, it atomically upserts one attention item and `alert_pending` record. A
pre-launch refusal invokes the same path rather than returning only a CLI error.

An explicit emergency operator override may launch without verified headroom only as
a narrowly scoped, expiring authorization recorded in the decision/audit model. It
must not silently restore the current fail-open default.

### 9.8 Dashboard capacity contract

Every account and seat shows:

- provider-global account label and each physical seat;
- live/cache/display source and trust state;
- age/staleness, last success, last attempt, and next retry time;
- all session/weekly/model-scoped windows and reset times;
- reserve line, routable/held decision, and exact hold reason;
- current project/epic/session occupants;
- duplicate identity or identity-swap warning;
- provider breaker and collection error state;
- a history chart and cap-hit/recovery events.

A gray or stale number is never styled like a healthy green route. Unknown stays
unknown; it is not rendered as zero usage or full headroom except for the explicitly
validated Grok absent-percent/present-period zero contract in §9.6.

### 9.9 Capacity regression tests

At minimum:

- remote cache-only and display-only Codex/Grok readings are visible but never
  routable;
- a live-probe interruption/restart cannot create a partial latest projection;
- nominally distinct registered account slots that resolve to one provider identity
  are quarantined;
- a duplicate home on one host is rejected, while two legitimate seats on different
  hosts sharing one account coalesce into one quota bucket and cannot
  double-lease a constrained account;
- throttling persists and honors provider retry time across restart;
- a stale observation remains visible but cannot pass the launch gate;
- an identity swap fences current routing and preserves the prior lineage history;
- a credential/config-home digest swap is quarantined even if a cached email happens
  to match;
- effective freshness fails closed after the collector stops, without requiring a
  new database write;
- malformed/out-of-range windows are rejected without erasing the last trusted fact;
- Codex protocol tests prove the initialize response precedes `initialized` and
  `account/read`/`account/rateLimits/read`;
- a Codex `account/read` identity mismatch is held;
- future/skewed captures are held, and reset passage requires a fresh probe rather
  than inferred recovery;
- two scoped limits with similar/trailing codenames remain distinct by stable upstream
  ID;
- Grok 401/expired auth, throttling, unknown schema, and missing identity are distinct
  held states;
- Grok old-log/new-auth composites are held, and monthly windows retain monthly
  semantics;
- a wedged remote probe times out without preventing other hosts/providers from
  completing their bounded-concurrency fold;
- concurrent routing chooses at most one winner for the last seat/account slot;
- all windows, not only a scalar weekly percentage, participate in reserve gating;
- dashboard and scheduler return the same routability reason;
- permuting seat result order produces the same account projection;
- live verified data beats display-only/stale data regardless of order;
- a failed seat cannot demote another seat's valid live account observation;
- newest capture wins only among equal trust/source rank;
- a lower-trust normal reading cannot clear a live verified critical limit;
- stale-critical and display-only-normal seats are both non-routable, with distinct
  visible reasons;
- a mid-generation window write failure cannot publish a ready seat or partial account
  generation.
- Codex missing its required weekly window is held; an explicitly absent optional
  session/scoped window is not synthesized as zero;
- Grok absent percent plus a valid active billing period is the one fixture-backed
  known-zero case; absent/malformed billing period is held;
- reviewer, builder, and operational selection all reject a stale or scoped-critical
  account through the same route gate;
- a shared-account seat with stale collection or lineage drift is held even when a
  sibling seat's coalesced account projection is fresh;
- pre-launch live-recheck failure creates a durable held dispatch and exactly one
  pool-capacity attention after threshold;
- zero routable seats in any required pool, including reserve-induced zero, produces
  one `capacity_pool_exhausted` alert with `alert_pending` durability;
- new Grok/Claude-family builds are rejected at admission when no distinct review
  family is configured;
- the operational Grok lease cannot consume the configured review reserve;
- under `capacity_routing_v2`, the legacy per-seat capacity writer cannot mutate the
  active `account_windows` generation.

## 10. Phased implementation plan

The phases are ordered by dependency, not visual appeal. Each stage must leave the
fleet runnable and all tests green. Feature flags gate actuation so schema and
read-model changes can land before behavior changes.

### Phase 0 — three separately gated foundations

Phase 0 has one ordered incident slice and two parallel foundations. Capacity and the
full actor-contract compiler are important, but neither is allowed to inflate or
block the first production repair.

#### P0.I — minimal incident slice (`epic_review_handoff_v2`)

This is the first shippable increment. Its pull requests merge in this order; each is
deployable with the flag off, and PR I.4 may activate before P0.CAP or P0.CONTRACT:

1. **I.1 — writer, identity, and schema.** Acquire the process-lifetime database
   writer lock; add opaque epic ID/slug/admission keys, delivery/review obligation,
   artifact/action tables, full-history action uniqueness, CAS/epoch claims, and the
   seeded global ledger. Create epic + obligation atomically in `AddEpicRun`. Do not
   alter current `epics.state` meanings.
2. **I.2 — owned artifact facts and legacy fences.** Add `headRefName` to the sweep
   read model while retaining fail-closed `EpicForHeadSHA` until it lands; define real
   CI green; exclude owned branches from `AdoptSweep`; absorb/supersede a pre-existing
   adopted job; tag native epic reviews out of stuck/janitor/liveness; stop epic
   sessions applying adoption labels.
3. **I.3 — native review materialization.** Implement
   `EnsureEpicReviewDispatched`, immutable-SHA dedup keys, review job/delivery-axis
   consistency, artifact-head supersession, and admission retry tests. This PR creates
   durable review work but leaves the flag in observe/shadow mode.
4. **I.4 — recovery, visibility, and independent alerting.** Enable the primary
   handoff and fact-based stall reconciler, delivery-wide state clocks/backstop,
   durable exact-project Interactor alert projection, supervised-loop
   heartbeat/watchdog, signed project-bound external dead-man ingress,
   `built · awaiting review dispatch`, and digest-sequence reads. Activate only after
   the exact #4950/#4951 crash test, poison-fact test, adoption-race test, and
   queued-capacity-wait test pass. I.4's activation registry is limited to admission,
   artifact, CI, and review-handoff rows; rework, conflict, merge, and cleanup rows
   remain alert-only until I.5/I.6 land. I.4 is additionally blocked on the live-lease
   `review_verdict_overdue` test.
5. **I.5 — durable actuator/rework seams.** Split observer from `tmux-send`, use remote
   session facts, make parking tear down panes, add fenced relaunch/rework plus
   dead-letter/flapping caps, and cover red/infra-red upstream stalls.
6. **I.6 — merge/conflict/cleanup totality.** Add exact-head merge, supersession,
   conflict→CI→independent-review, remote+proxy cleanup, and symmetric fact-based
   backstops for rework/merge/cleanup. I.6 cannot activate until the registry covers
   `conflict_resolution` and `merged`, and dead-lettered merge/cleanup actions can be
   re-armed in place.

P0.I exit gate:

- lost admission acknowledgement produces exactly one epic/delivery;
- interruption after green fact commit produces one native review and one alert;
- every non-terminal delivery/CI state has an executable next-action/hold registry;
- arbitrary/adoption-labeled PRs cannot create a second review of an owned epic;
- a native review capacity wait cannot be moved by legacy stuck/janitor logic;
- a dead reconciler or dead-letter emits push-visible attention independently of SSE;
- no rebuilt, conflict-resolved, or superseded head merges without fresh CI and a
  distinct-family verdict.

#### P0.CAP — trustworthy capacity and unified route gate (parallel)

Deliverables:

- deploy the authenticated sanitized live collector on every intended host, with a
  written onboarding/health runbook and a readiness check;
- add expected identity/credential/lineage, per-seat observations, deterministic
  account reduction, atomic generations, history, backoff, and single-flight;
- define provider required-window/unknown semantics and captured fixtures;
- make builder/reviewer/operational selectors call the one fail-closed gate;
- disable the legacy per-seat projection writer and freshness-free
  `worker_accounts.usage_pct` route under the flag;
- add prelaunch held-dispatch semantics, `capacity_pool_exhausted` reconciliation,
  alert-pending/push, operational-Grok reserve, and distinct review-family admission;
- validate reserve percentage against real pool size in shadow before activation.

P0.CAP exit gate:

- every production seat has a fresh identity-bound live observation from its own
  host; cache/display data is never routable;
- shared accounts are coalesced without one good seat masking another seat's stale or
  drifted lineage;
- zero eligible seats in build, review, or operations raises one durable pushed alert;
- Grok absent-percent semantics and CI-green semantics pass fixture/contract tests;
- Grok/Claude builds cannot enter a fleet with no distinct review family.

#### P0.CONTRACT — actor documentation and recovery compiler (parallel)

Build §18's machine-readable protocol, role cards, recovery registry, compatibility
handshake, documentation-as-code checks, and dashboard help in parallel. P0.I ships
with checked-in bounded manual role/recovery contracts for the touched actors; the
compiler replaces those artifacts only after its own parity tests pass. It is not a
co-requisite of I.1–I.4.

#### P0.COMPLETE — full unattended lifecycle

After P0.I is active and P0.CAP/P0.CONTRACT are separately green, enable the remaining
builder launch/re-home, review, rejection, merge, conflict, cleanup, contract strict,
and capacity strict flags in canary order. Phase 0 is complete when one admitted epic
can proceed unattended through merge/cleanup and every failure is self-recovering or
durably held and pushed.

### Phase 1 — dashboard-first human interaction

Phase 1 makes the Flowbee dashboard the normal home screen and turns human judgment
into durable, versioned workflow input that promotes automatically when its real
gates are satisfied. Tmux remains the agent transport and debugging fallback.

#### P1.A — typed decision and work-intent domain

Deliverables:

- `decision_requests` and append-only `decision_responses` migrations;
- `work_intents` migration, state machine, source-message/version binding, and
  idempotent admitted-epic link;
- closed request/response kinds and JSON schemas;
- artifact/version/hash binding with stale-response rejection;
- project/epic/evidence routes and audit events;
- authenticated APIs to view, answer, approve, request changes, defer, and deny;
- idempotency keys for responses and explicit supersession/cancellation;
- downstream acknowledgement chain state;
- Interactor, Orchestrator, dashboard, and human-facing role/recovery cards generated
  from the same §18 protocol contract;
- automatic promotion when definition and required typed gates are satisfied;
- a startup/periodic promotion reconciler with fenced route leases and a distinct
  stalled-promotion alert.

Exit gate:

- a plan/design changed after display cannot consume the old approval;
- retrying a browser submission creates one response;
- every open request and work intent has an owner, route, priority, and next action;
- a sufficiently defined request with no unsatisfied gate reaches its Orchestrator
  without a second human instruction.

#### P1.B — global Needs You inbox

The global dashboard adds one prioritized inbox with:

- counts by urgency, project, type, and age;
- `question`, `plan review`, `design review`, `authorization`, and `exception` cards;
- project/epic provenance and who is waiting;
- concise prompt, structured choices, recommended option when appropriate, and the
  consequence of delay;
- evidence and context drawer with immutable artifact links/hash/version;
- actions: `Approve`, `Request changes`, `Answer`, `Defer`, and `Deny` as permitted by
  type;
- acknowledgement, due/deferred time, and activity trail;
- clear stale/superseded states that cannot be acted on;
- keyboard-accessible, responsive, Tailnet-safe operation.

Priority ordering is deterministic: explicit severity, blocking status, due time,
age, then stable ID. One project cannot visually bury another project's urgent human
request.

Exit gate:

- the human can resolve every supported request without opening tmux;
- the UI never renders a free-form answer as authorization;
- a refreshed or second browser sees the same durable state.

#### P1.C — per-project Interactor workspace

Each project workspace provides:

- the actual focused human↔Interactor conversation surface: durable thread history,
  message composer, streamed Interactor response, delivery status, and exact project
  route;
- thread focus on a project, epic, artifact, or decision without losing the stable
  project conversation identity;
- current goals, priorities, epics, decisions, plans, and designs;
- the Interactor's last human-facing report and next proposed update;
- open questions grouped with the artifact/evidence they concern;
- approved/rejected/superseded decision history;
- delivery pipeline and capacity state relevant to the project;
- links to Orchestrator plan artifacts and Flowbee operational evidence;
- an audited "open terminal/debug" fallback, not a primary call to action.

The dashboard is a presentation/workspace for the Interactor layer, not a bypass
around it. The Interactor authors the human-readable framing; Flowbee stores and
renders it. Deterministic facts remain visibly distinct from agent-authored analysis.
Conversation can automatically create and refine a durable work intent. When policy
requires a separate plan, design, authorization, or exception decision, the
Interactor creates that typed request; chat prose cannot silently satisfy it. Once
the current intent is sufficiently defined and all such gates are satisfied, the
Interactor promotes it automatically. The UI shows the promotion and acknowledgement
timeline but does not make the human press a required "send to Flowbee" button.

Exit gate:

- routine focused conversation with the project Interactor happens from the dashboard
  with streamed responses and durable history, without opening tmux;
- reconnect/reload resumes the same thread without losing or duplicating messages;
- conversational prose alone cannot advance an approval or authorization gate;
- a defined and authorized work intent promotes automatically without a terminal or
  extra human plumbing command.

#### P1.D — notification and acknowledgement chain

Deliverables:

- durable notification actions to the correct project Interactor;
- push-to-wake only from closed templates and safe pane states;
- Interactor acknowledgement of the human response;
- durable routing to the paired Orchestrator and its acknowledgement;
- durable routing of the resulting instruction/authorization to Flowbee and its
  acknowledgement;
- automatic `work_intent → epic contract → admitted epic` delivery and reverse
  acknowledgement, idempotent across every actor restart;
- retries, timeout/escalation, and replacement-session fencing at every hop;
- dashboard visualization of "answered; waiting for Interactor/Orchestrator/Flowbee"
  rather than pretending the response was already applied.

The human's original structured response is immutable. Interactor or Orchestrator
interpretation is a separate signed artifact that may add context but cannot rewrite
the answer. A satisfied gate immediately re-arms its blocked work intent; it does not
create another human "go" gate.

#### P1.E — authorization, audit, and safe rollout

Deliverables:

- authenticated human identity/session and CSRF protections;
- role/action matrix and project scope checks;
- explicit, expiring, least-privilege authorization records for destructive or
  fail-open exceptions;
- confirmation requirements proportional to consequence;
- immutable audit export showing request hash, response, actor, acknowledgements, and
  resulting state transition;
- read-only shadow mode, then one-project canary, then dashboard-default rollout;
- terminal fallback runbook that does not bypass durable decision recording.

Exit gate:

- no UI response can authorize a changed artifact or a broader action than displayed;
- the complete human→Interactor→Orchestrator→Flowbee chain is inspectable;
- no eligible work intent can remain idle solely because the human did not name
  Flowbee or repeat an approval;
- normal operations no longer require a human to watch a tmux pane.

### Phase 2 — multi-project portfolio and shared fleet

Phase 2 generalizes one reliable project into a portfolio while preserving one
logical Flowbee authority and shared build/review capacity.

#### P2.A — project model and default-project backfill

Deliverables:

- `projects` and `project_repos` tables;
- stable default project containing all existing repositories/epics;
- nullable-add/backfill/validate migration of `project_id` across durable state;
- project-scoped Interactor and 1:1 Orchestrator registrations;
- explicit epic repository set and one delivery repository;
- archive/pause semantics that do not delete history;
- API compatibility mapping for clients that omit project during the migration
  window.

Exit gate:

- every live work intent, epic, action, attention item, decision, artifact, audit
  event, and cost is attributable to exactly one project;
- repo remains independently identifiable and policy-bearing;
- existing single-project installs behave unchanged under the default project.

#### P2.B — route and namespace isolation

Deliverables:

- project ID in all commands, worker/session grants, Tmux Driver targets, SSE events,
  API resources, and audit payloads;
- tmux/session/worktree/action names prefixed with a collision-safe project key;
- exact response/escalation routing to the project's Interactor and Orchestrator;
- project-scoped secrets/policy references without copying credentials into rows;
- guards against cross-project epic IDs, dedup keys, WIP markers, or artifact links;
- project-filtered logs and traces.

Exit gate:

- two projects may use the same human epic slug without sharing a session, action,
  decision, branch, or alert identity;
- a response from project A cannot advance project B.

#### P2.C — global portfolio and per-project workspaces

The global dashboard shows:

- projects with health, priority, active/parked epics, delivery states, and oldest
  blocker;
- shared builder/reviewer/account/seat capacity and current allocation;
- global throughput, merge, recovery, and fairness indicators;
- a unified Needs You inbox with project provenance;
- project/repository circuit breakers and provider incidents;
- cross-project scheduling reasons and reset times.

Drilling into a project opens the Phase 1 workspace with project-scoped epics,
decisions, plans, designs, capacity share, costs, artifacts, and audit. Repository
filters remain available inside a project; they never replace project navigation.

#### P2.D — fair shared-capacity scheduler

Use a deterministic weighted fair scheduler over **projects**, then select eligible
epics within the winning project by dependency, priority, and age. A weighted deficit
round-robin or equivalent replayable algorithm should provide:

- configurable project weights and optional concurrency caps;
- age credit so a continuously eligible low-weight project cannot starve;
- separate pools/constraints for the configured build and review capabilities
  (Codex/Grok in the default profile);
- priority for review/rework needed to release parked delivery throughput, without
  consuming the builder's persistent affinity as an active compute slot;
- account-global quota and seat-local concurrency gates from Phase 0;
- no project-specific LLM judgment inside the scheduling decision;
- a legible "why not scheduled" explanation for every eligible epic.

Fairness metrics include service share vs weight, oldest eligible wait, consecutive
dispatches by project, review wait, and starvation-bound violations.

Exit gate:

- load tests with one noisy project still schedule sustained eligible work from every
  other project within the defined bound;
- reviewers cannot be consumed as builders or vice versa;
- account/seat exhaustion in one family is visible and does not corrupt other pools.

#### P2.E — project fault isolation and circuit breakers

Deliverables:

- per-project and per-repository breakers for CI outage, GitHub errors, merge
  incidents, repeated action failures, and policy violations;
- provider/account breakers remain global only when the underlying dependency is
  genuinely global;
- breaker scope attached to every hold reason and dashboard banner;
- automatic half-open probes and audited operator overrides;
- queue processing that skips held projects without stopping the global ticker;
- resource budgets so one project's pathological reconciliation set cannot monopolize
  the loop.

Exit gate:

- a broken CI configuration or GitHub outage for one repository/project cannot stall
  unrelated projects;
- a global provider outage is represented once and affects only dependent work;
- breakers survive restart and close only from reconciled recovery facts.

#### P2.F — logical-authority resilience and rollout

SQLite retains the Phase-0 enforced single-writer process model: an OS advisory lock
next to the database prevents overlapping authorities, and all legacy/new outbox
drains use atomic claim-with-epoch. The process runs as a supervised replaceable
service with restart recovery and no session-owned state. Operational agent sessions
are replaceable fenced claimants.

Deliverables:

- startup recovery order and readiness gate;
- leader/process incarnation in metrics and audit;
- watchdog/service restart with bounded recovery objective;
- database backup/integrity/restore drills;
- optional future store abstraction for replicated authority without changing the
  state-machine contract;
- staged enablement: default project, two-project canary, portfolio shadow fairness,
  then active shared scheduling.

Exit gate:

- killing the Flowbee process or every operational agent session loses no admitted
  epic, decision, action, lease fence, or review obligation;
- after restart, one authority resumes and converges without duplicate effects;
- the human sees one coherent global portfolio, not one dashboard per process.

### 10.1 Key differences between the phases

| Concern | Phase 0 | Phase 1 | Phase 2 |
| --- | --- | --- | --- |
| Primary objective | pipeline continuity and safe capacity | dashboard-first human judgment plus automatic intent promotion | many projects on one shared authority/fleet |
| Ownership | epic in default/single project | unchanged | explicit project → epics; project ≠ repo |
| Human interface | operational states and alerts | typed inbox, plans/designs/questions/approvals | global portfolio + project workspaces |
| Agent route | existing single route, made durable | Human ↔ Interactor ↔ Orchestrator ↔ Flowbee acknowledgements; no human plumbing step | one Interactor/Orchestrator pair per project |
| Scheduling | trustworthy seats and one epic lifecycle | display/explain scheduling | weighted fair scheduling across projects |
| Failure isolation | per epic/repo/provider basics | per decision/route | per project/repo breakers and loop budgets |
| Terminal role | Tmux transport/debug | dashboard is normal home | dashboard is global portfolio and project switcher |

Phase 1 does not require multi-project scheduling, but its schema and APIs carry a
default `project_id` so Phase 2 does not require another semantic rewrite. Phase 2
does not weaken Phase 1's single global Needs You inbox; it adds project provenance,
filtering, and exact routes.

## 11. Configuration, API, and UI contracts

### 11.1 Proposed configuration

All duration values are validated, documented, and available through explicit
environment overrides. Proposed keys/defaults are:

| key | proposed default | purpose |
| --- | ---: | --- |
| `epic_review_handoff_v2` | `false` during rollout | independently enables the P0.I incident slice |
| `capacity_routing_v2` | `false` during rollout | independently enables P0.CAP fail-closed routing after fleet onboarding |
| `epic_reconcile_interval_s` | `45` | artifact and delivery convergence cadence |
| `review_handoff_stall_s` | `300` | maximum review-eligible missing-dispatch window before repair+alert |
| `review_verdict_stall_s` | `900` | maximum live reviewer window without verdict/evidence progress |
| `artifact_stall_s` | `600` | builder parked without an owned PR before recovery+alert |
| `ci_pending_stall_s` | `900` | current PR without real green/red fact progress before attention |
| `capacity_pool_exhausted_s` | `300` | eligible work at zero routable seats before durable alert |
| `auto_recovery_max_per_head` | `3` | cap before a repeated seam enters a needs-human/flapping hold |
| `reconciler_heartbeat_grace_s` | `180` | in-process watchdog threshold for a missing loop heartbeat |
| `epic_action_ack_s` | `360` | default send-to-processing acknowledgement budget; overridable by kind |
| `epic_action_max_attempts` | `5` | bounded automatic retries before durable dead letter/escalation |
| `epic_action_retry_min_s` | `15` | initial retry delay |
| `epic_action_retry_max_s` | `600` | bounded exponential backoff ceiling |
| `capacity_probe_interval_s` | `300` | normal background live-probe cadence |
| `capacity_live_max_age_s` | `900` | hard maximum live observation age allowed at route time |
| `capacity_prelaunch_max_age_s` | `180` | age that triggers a joined/on-demand recheck before launch or resume |
| `capacity_probe_timeout_s` | `20` | one provider call/collector attempt budget |
| `capacity_probe_host_concurrency` | `4` | bounded hosts probed concurrently |
| `capacity_probe_provider_concurrency` | `2` | bounded calls per provider |
| `capacity_history_retention_days` | `30` | append-only usage history retention |
| `capacity_history_min_interval_s` | `60` | sampling floor for unchanged history |
| `capacity_reserve_pct` | `10` | default minimum headroom in every relevant window |
| `decision_ack_timeout_s` | `600` | per-hop Interactor/Orchestrator/Flowbee acknowledgement budget |
| `work_intent_route_stall_s` | `300` | eligible promotion hop deadline before repair+alert |
| `actor_contract_strict` | `true` | refuse leases/routes to an actor that has not acknowledged the compatible contract |
| `control_plane_lock_path` | `<database>.writer.lock` | process-lifetime exclusive writer lock |
| `control_alert_ingress_secret_file` | secret environment value | owner-only HMAC key used by the independent watchdog to submit a project-bound incident to Flowbee; human delivery is always through that project's Interactor |
| `external_watchdog_max_age_s` | `180` | independent dead-man threshold for last completed reconcile pass |
| `project_default_id` | `default` | single-project migration/backfill identity |
| `project_starvation_bound_s` | `900` | alert bound for continuously eligible project work |

The exact capacity freshness must be no more than a small multiple of the collector
cadence. Validation rejects contradictory timings, such as a handoff stall threshold
shorter than one reconciliation pass or a freshness window longer than the provider
observation safety policy.

Role configuration is capability/family based, for example default build and review
pools, not hard-coded provider columns. The shipped profile can map build to Codex and
review to Grok while preserving provider-neutral scheduling.

Feature flags are one-way safe:

- disabling actuation stops new sends/merges but continues reconcile-in, durable state,
  alerts, and read models;
- disabling auto-repair leaves the stall visible and pages, rather than hiding it;
- no flag returns arbitrary PR adoption to the v2 path.

### 11.2 Public read API

The public, versioned read model includes:

```text
GET /v1/portfolio
GET /v1/projects
GET /v1/projects/{project_id}
GET /v1/projects/{project_id}/epics
GET /v1/projects/{project_id}/work-intents
GET /v1/epics/{epic_id}/delivery
GET /v1/epics/{epic_id}/artifacts
GET /v1/epics/{epic_id}/audit
GET /v1/decisions?state=open&project_id={project_id}
GET /v1/conversations/{thread_id}/messages
GET /v1/actors/{actor_id}/contract
GET /v1/recovery?project_id={project_id}&epic_id={epic_id}
GET /v1/capacity
GET /v1/summary
GET /v1/events
```

Every response has stable ordering, `[]` rather than `null`, a schema version, and a
digest sequence. Large evidence, plan, design, review, and transcript bodies are
immutable artifact links with hashes. List endpoints support bounded pagination and
field projection where physical/dashboard clients need small payloads.

ETag/304 is the polling truth. SSE emits named, project-aware wake-up events with the
latest digest sequence and periodic keepalives. Clients must refetch after a gap and
must not treat SSE as an event ledger.

### 11.3 Authenticated write API

Orchestrator/Flowbee operations:

```text
POST /v1/projects/{project_id}/epics
POST /v1/projects/{project_id}/work-intents
POST /v1/work-intents/{id}/pause
POST /v1/work-intents/{id}/resume
POST /v1/work-intents/{id}/cancel
POST /v1/epics/{epic_id}/pause
POST /v1/epics/{epic_id}/resume
POST /v1/epics/{epic_id}/abandon
POST /v1/epics/{epic_id}/retry-action
POST /v1/decisions
POST /v1/conversations/{thread_id}/messages
```

Human dashboard operations:

```text
POST /v1/decisions/{id}/view
POST /v1/decisions/{id}/answer
POST /v1/decisions/{id}/approve
POST /v1/decisions/{id}/request-changes
POST /v1/decisions/{id}/defer
POST /v1/decisions/{id}/deny
POST /v1/conversations/{thread_id}/messages
```

Collector/agent internal operations remain on a private authenticated tier:

```text
POST /v1/collectors/{host_id}/capacity
POST /v1/actors/{actor_id}/contract-ack
POST /v1/work-intents/{id}/route-ack
POST /v1/actions/{id}/receipt
POST /v1/actions/{id}/ack
existing fenced review lease / heartbeat / result endpoints
```

Every mutating request carries an idempotency key and, where relevant, expected state
version, artifact hash, action epoch, or lease epoch. Responses distinguish:

- `409` stale/superseded/fenced;
- `412` expected version/hash no longer current;
- `422` invalid transition or schema;
- `429` bounded provider/project backpressure with retry time;
- `503` scoped breaker/unavailable dependency with durable hold reason.

No generic "set state" endpoint exists. APIs request domain transitions, and the
store enforces legal edges and invariants. There is intentionally no human-only
"promote" or "send to Flowbee" endpoint. The Interactor transition and reconciler own
normal promotion; human endpoints can pause/cancel or satisfy a typed gate.

### 11.4 Authentication and authorization

- Orchestrator credentials are project-scoped and can submit/plan only for registered
  project repositories.
- Interactor credentials are project-scoped and can frame conversations/decisions but
  cannot merge or widen an authorization.
- Reviewer/builder/collector credentials carry only their role and registered
  host/session scope; workers receive no GitHub credentials.
- Human dashboard writes require an authenticated Tailnet/session identity, CSRF
  protection, and an action-specific permission.
- High-consequence responses show the exact scope and artifact hash and may require a
  second confirmation. Emergency fail-open capacity overrides expire and are
  single-purpose.
- Read exposure is configurable. Loopback/Tailnet open reads may remain convenient,
  but multi-project names, conversation content, evidence, and usage identity require
  explicit privacy review before being left unauthenticated.

### 11.5 Global dashboard information architecture

The home screen answers, in order:

1. **What needs me?** Unified typed Needs You inbox, highest priority first.
2. **Is work flowing?** Counts and oldest age for building, awaiting CI, awaiting
   review dispatch, review queued, in review, rework, merge, conflict, and cleanup.
3. **Where is the risk?** Stalls/recoveries, breakers, CI incidents, stale capacity,
   identity drift, and dead-letter actions.
4. **What capacity is usable?** Build/review seats, accounts, windows, trust/source,
   resets, allocation, and fairness.
5. **What changed?** Recent merges, verdicts, decisions, recoveries, and audit events.

The home/project views also show defined work moving through
`Interactor → Orchestrator → Flowbee admission`, including a reason and owner for any
hold. This is an audit/diagnostic timeline, not a required manual submit control.

The exact incident state is a first-class lane/card:

```text
built · awaiting review dispatch
PR #4950 · CI green · eligible 7m ago · no review action/reviewer
auto-recovery: pending | recovered at …
```

It is not grouped under Completed and is not described merely as "review queue."
Once a durable review task exists it becomes `review queued`; once a lease is live it
becomes `in review`.

### 11.6 Project workspace

The workspace combines:

- focused, durable human↔Interactor conversation with streamed responses;
- project goals, plans, designs, decisions, and issue-reference inputs;
- epic pipeline board and artifact/review evidence;
- the paired Interactor/Orchestrator route and acknowledgement health;
- captured work intents and their automatic promotion/admission timeline;
- project capacity share, waits, breakers, costs, and recent audit;
- a safe terminal/debug link with target identity, available as fallback.

Conversation and deterministic state use distinct visual treatments. Agent analysis
is labeled with author/model/incarnation and artifact version. Deterministic facts
such as CI green, live reviewer lease, and merge commit are never paraphrased into an
unverifiable badge.

### 11.7 Needs You interaction details

Each decision card/drawer shows:

- request type, project, epic, requester, priority, age, and blocking impact;
- the exact question or review subject;
- current artifact version/hash and a changed/superseded warning;
- structured options and consequences;
- evidence/context tabs with immutable links;
- prior related decisions and relevant conversation thread;
- permitted actions only;
- response and downstream acknowledgement timeline.

`Approve` and `Request changes` are available only for reviewable versioned artifacts.
`Answer` follows the request's typed schema. `Defer` requires a time/condition and
keeps the item visible in a deferred section. Free-form comments may accompany a
structured action but never replace it.

## 12. Verification strategy and exact regression matrix

### 12.1 Exact production-drop regression

The required integration test uses a real temporary SQLite store, fake clock, fake
GitHub facts, fake Tmux Driver, and a newly constructed reconciler after the simulated
crash:

1. Admit epic `E` for project `P` and repo `R`. Assert the epic, delivery, branch,
   builder family, and `review_required=true` commit together.
2. Launch the builder, then park it by tearing down both its remote pane and any
   local attach proxy while preserving only worktree/branch/builder identity/scope
   affinity and releasing active compute.
3. Reconcile an open, same-repository, non-draft PR on `E`'s exact branch with current
   SHAs and real green CI as defined in §6.2. Record `ci_green_observed_at`.
4. Use the named `post_eligible_fact_commit_pre_materialize` injectable hook to stop
   after the artifact/state transaction commits but before materialization/delivery.
5. Close the database/process. There is no reviewer verdict, action, job, or lease.
6. Advance the fake clock beyond `review_handoff_stall` from the observed green fact
   and reopen the same database
   with a fresh reconciler/dispatcher instance.
7. Tick startup/periodic reconciliation.
8. Assert exactly one native epic review job/action exists, linked to `E` and the
   current SHAs; the projection is normalized to `review_queued`; recovery count is
   one; one durable alert/audit record exists.
9. Present two eligible review claimants concurrently. Assert one wins the atomic
   lease and the other loses; the delivery becomes `in_review`.
10. Restart and repeat sweeps. Assert no second review/action/alert/recovery increment.

This test reproduces the actual failure: interruption between "build/CI done" and
"review dispatched," followed by a new process with no transcript memory.

### 12.2 Crash-window matrix

Add failpoints and tests at every durable seam:

| crash point | required recovery |
| --- | --- |
| before admission commit | no epic/delivery/action exists |
| after admission, before launch | launching recovery/rollback sees durable obligation |
| builder parked, PR never appears | artifact backstop refetches/relauches then raises `artifact_never_produced` |
| PR open, CI remains none/pending | CI backstop raises `ci_never_green`; delivery stays non-complete |
| CI red after park | one SHA-keyed builder-rework action is recovered |
| CI infra-red after park | bounded refetch/retry then `KindCIInfraIncident` |
| after artifact fetch, before fact commit | next sweep refetches; no partial facts |
| after eligible fact commit, before review materialization | stall/startup reconciler creates one review |
| after review materialization, before wake/SSE | queued row is claimable; no duplicate materialization |
| labeled epic PR adopted pre-CI, then green | adopted job is absorbed/superseded; one live review exists |
| native review waits without eligible family | legacy stuck/janitor skips it; capacity return enables one claim |
| after action `delivering`, before terminal send | recovery verifies then sends once |
| after `tmux-send`, before receipt commit | remote fact/action hash determines ack/nudge vs safe retry; status 4 never resends payload |
| after reviewer claim, before heartbeat | lease expires/requeues; stale reviewer fenced |
| after request-changes commit, before builder send | durable rework action resumes same builder |
| rework action missing/dead-lettered | backstop verifies/re-arms same row or holds+alerts once |
| after approval commit, before merge call | one merge action resumes |
| head advances while merge action is live | old action becomes superseded; delivery returns to awaiting CI |
| after GitHub merge, before local fact commit | reconcile observes merge commit; no second merge |
| after merge fact, before cleanup | cleanup action resumes against explicit targets |
| merge or cleanup action missing/dead-lettered | fact backstop holds+alerts without duplicate effect |
| after human response, before Interactor acknowledgement | response remains immutable and delivery retries |
| after work intent becomes ready, before Orchestrator delivery | promotion reconciler restores one route action |
| after epic admission, before Orchestrator receives acknowledgement | idempotency lookup links the existing epic; no duplicate admission |
| midway through capacity generation | old generation stays active; no partial ready seat |
| required pool has zero routable seats | one durable `capacity_pool_exhausted` alert is pushed after threshold |
| poison artifact/capacity row panics one iteration | row is quarantined; loop heartbeat continues; watchdog stays healthy |
| process crashes before readiness on every restart | external dead-man pushes an alert independently of Flowbee |

### 12.3 Ownership and artifact tests

- exact branch/repo/project match binds the PR;
- near-miss branch, fork head, repo mismatch, project mismatch, label-only PR, issue
  reference, commit author, and arbitrary green PR do not bind;
- two candidate PRs for one branch raise ambiguity and dispatch neither;
- PR binding is stable across restart;
- a replacement after closed-unmerged requires an explicit audited operation;
- head/base movement increments artifact version and invalidates current eligibility,
  lease, and verdict;
- two distinct heads/bases derive distinct action keys even if artifact-version
  projection is stale;
- all-skipped, missing-required-check, or truncated check contexts never become real
  green;
- stale approval cannot enqueue merge;
- an ordinary PR remains untouched even when all reviewers are idle.

### 12.4 Builder/review/merge tests

- builder reported done preserves affinity but tears down its remote/proxy panes;
- parked epic frees active compute capacity but still blocks overlapping scope;
- review rejection reacquires capacity and relaunches a new fenced incarnation on the
  same worktree/branch/builder identity affinity;
- same builder/reviewer identity or family cannot win review;
- configured independent family can win;
- no independent capacity leaves one queued review and fires a capacity alert, not
  repeated redispatch;
- legacy stuck/janitor/liveness cannot move a native epic review job to
  `needs_human`, and linked delivery/job axes cannot disagree after reconciliation;
- a pre-CI adopted/labeled owned PR becomes exactly one native review when green;
- a review action pending/delivering/queued/leased suppresses stall repair even if the
  delivery state projection is wrong;
- a missing execution/action is repaired even if the delivery projection is not
  `awaiting_review_dispatch`;
- review evidence includes required test execution and exact artifact SHAs;
- conflict resolution forces new CI and independent review;
- rebuilt/conflict-resolved/superseded heads cannot reach merge without new CI and a
  distinct-family verdict;
- merge requires current CI/verdict/content gates and records one merge commit;
- cleanup deletes only registered targets and releases scope only after merge/abandon.

### 12.5 Dashboard and decision tests

- CI-green missing dispatch renders `built · awaiting review dispatch`, never
  Completed;
- CI-pending renders `built · awaiting CI`;
- durable job with no lease renders `review queued`; live lease renders `in review`;
- builder `parked` does not inflate complete counts;
- recovery alert appears once, remains until claim, and persists in history;
- digest/ETag advances for delivery/action/decision/capacity changes;
- decision approval fails on changed artifact hash/version;
- browser retry creates one response;
- defer requires a time/condition and reappears correctly;
- human response timeline shows Interactor→Orchestrator→Flowbee acknowledgements;
- conversation messages persist, stream in order, resume after reconnect, and cannot
  themselves satisfy a typed approval;
- an ordinary executable request creates one versioned work intent and automatically
  reaches the paired Orchestrator when no separate decision gate is required;
- a work intent that requires plan/design approval waits for the typed response and
  promotes immediately after the current version is approved, with no second human
  "go" message;
- Interactor/Orchestrator restart at every promotion hop drains the durable intent,
  and an uncertain admission acknowledgement links one existing epic;
- undefined intent creates a concrete product question, never a generic request to
  use Flowbee;
- an incompatible or unacknowledged actor contract prevents work delivery and raises
  a visible contract hold;
- project A credentials/routes cannot read or mutate restricted project B resources.

### 12.6 Multi-project fairness and isolation tests

- all legacy records backfill into the default project without semantic change;
- same epic slug in two projects cannot collide in DB/action/tmux/worktree namespaces;
- weighted scheduling converges to configured shares under sustained load;
- age credit schedules a continuously eligible low-weight project within the
  starvation bound;
- review/rework that releases delivery throughput is prioritized without counting a
  parked builder as active compute;
- project/repo breaker skips only its scope;
- a pathological project reconciliation set respects work/time budgets;
- one project's CI outage, route failure, or decision backlog does not stall another;
- global account outage affects only work requiring that account/family;
- unified Needs You ordering retains project provenance and urgent cross-project
  fairness.

### 12.7 Property and invariant tests

Use table/property tests for:

- every delivery state and CI substate has a registered next action/owner/due clock or
  visible hold reason;
- applying the same fact/action/result twice is idempotent;
- state version and lease/action epochs are monotonic;
- at most one live review execution and merge action exist for an epic/artifact;
- a historical acknowledged/cancelled/dead-lettered action dedup key cannot be
  inserted again, except a pre-effect `cancelled_superseded` row may be excluded
  from the live index for an H1→H2→H1 revisit;
- H1→H2→H1 force-push/base-reset churn leaves exactly one live dispatch and no
  verdict from the superseded H1 action;
- a terminal/abandoned delivery never reactivates from stale input;
- no verdict verifies a different head/base;
- no action targets a different project/session incarnation;
- no account projection depends on seat enumeration order;
- no lower-trust capacity observation improves routability;
- no typed decision accepts a stale subject hash.
- two serve processes cannot both become ready writers, and a legacy outbox claim
  has exactly one epoch winner;
- a poisoned fact quarantines only its row while reconciler heartbeat and unrelated
  rows continue;
- zero routable capacity, prelaunch recheck failure, and remote channel loss each
  produce a durable hold/attention rather than an aged retry;
- observer permissions cannot invoke `tmux-send`, and local proxy loss cannot create
  a second remote session;
- a legacy `UpsertAccountLimits` path cannot mutate a v2 account generation;
- migration validation rejects a new ladder number that is not `max(main)+1`.
- a parked delivery has no live session and can only re-enter work through a fenced
  relaunch with a new session incarnation;
- an owned epic branch is never adopted by the legacy label sweep or legacy stuck/
  janitor state machine;
- a required capacity pool at zero routable seats produces one durable attention
  item before any retry loop can age silently.

The invariant property test iterates the executable recovery registry rather than a
hand-maintained subset. CI fails when a new non-terminal state, CI state, action kind,
or recovery code has no owner, due policy, failpoint, attention/dashboard state, and
named acceptance test.

### 12.8 Executable recovery traceability

Recovery tests use an injected clock plus a closed `ReconcileHooks` interface whose
named post-commit hooks are enabled in test builds. Production hooks are no-ops. Tests
may stop an instance at a seam; they may not mutate tables directly to approximate a
crash window.

The checked registry contains at least:

| invariant / recovery code | owner | failpoint / named acceptance test |
| --- | --- | --- |
| admission atomicity / `epic_admission_outcome_uncertain` | core | `post_admission_pre_ack` / `TestAdmissionRetryWithFreshSlugReturnsExistingEpic` |
| durable next action / `builder_launch_stalled` | launch reconciler | `post_admission_pre_launch` / `TestAdmissionLaunchCrashRedispatchesExactlyOnce` |
| artifact progress / `artifact_never_produced` | artifact reconciler | `post_park_pre_artifact` / `TestAwaitingArtifactStallAlerts` |
| CI progress / `ci_never_green` | artifact reconciler | `post_pr_bind_pre_ci_terminal` / `TestAwaitingCINeverGreenAlerts` |
| CI red / `ci_red_on_epic_pr` | rework reconciler | `post_ci_red_pre_rework` / `TestCIRedDispatchesBuilderRework` |
| review handoff / `review_dispatch_missing` | review reconciler | `post_eligible_fact_commit_pre_materialize` / `TestDroppedReviewHandoffRecoveredExactlyOnce` |
| review capacity / `review_capacity_unavailable` | capacity/review reconciler | `post_review_queue_pre_claim` / `TestCapacityStarvedEpicReviewNeverEscalatesToNeedsHuman` |
| reviewer verdict / `review_verdict_overdue` | review lease reconciler | `review_lease_heartbeat_without_fact` / `TestHungRenewingReviewerIsFencedAndRequeued` |
| conflict / `conflict_resolution_stalled` | conflict reconciler | `post_conflict_ack_pre_new_head` / `TestConflictResolutionWithoutHeadAlerts` |
| merged cleanup / `cleanup_overdue` | cleanup reconciler | `post_merge_fact_pre_cleanup` / `TestMergedStateAlwaysHasCleanupAction` |
| action uncertainty / `action_delivery_uncertain` | action executor | `post_tmux_send_pre_receipt` / `TestTmuxSendStatus4VerifiesOrNudgesWithoutResend` |
| artifact supersession / `artifact_advanced` | artifact reconciler | `post_merge_claim_pre_effect` / `TestHeadAdvanceCancelsOldActionsAndReturnsAwaitingCI` |
| capacity outage / `capacity_pool_exhausted` | capacity reconciler | `post_route_refusal` / `TestZeroRoutableRequiredPoolRaisesOneDurableAlert` |
| merge uncertainty / `merge_outcome_uncertain` | merge reconciler | `post_merge_effect_pre_fact` / `TestMissingMergeActionRecovered` |
| cleanup / `cleanup_overdue` | cleanup reconciler | `post_merge_fact_pre_cleanup` / `TestMissingCleanupActionRecovered` |
| dead-letter rearm / `action_dead_lettered` | effect backstop | `post_dead_letter_before_rearm` / `TestDeadLetteredMergeAndCleanupRearmSameAction` |
| reconciler liveness / `reconciler_dead` | watchdog | `panic_one_reconcile_item` / `TestPoisonFactDoesNotKillReconcilerLoop` |

`TestEveryHardInvariantHasExecutableCoverage` and
`TestRecoveryContractHasNamedFailpointAndAcceptanceTest` compare §4/§18 against this
registry. `TestAdmissionTransfersPipelineContinuityToCore` and
`TestActorAuthorizationRejectsManualReviewDispatch` prove no conversational actor can
recreate the lost in-memory handoff design.

All packages, acceptance tests, migration/ladder checks, architecture checks, and
provider-neutrality lint must stay green at every stage.

## 13. Rollout, migration, and observability

### 13.1 Additive migration and backfill

Migration allocation is merge-ordered, not reservation-ordered:

1. On the current baseline, `0032` is the next eligible number. Never create the
   superseded/unlanded `0029` or `0030` files.
2. Each schema PR runs `flowbee migration reserve <slug>` and commits the `.sql` plus
   its `LADDER.md` row in the same PR.
3. Immediately before merge, rebase on main and renumber to exactly
   `max(main)+1` if another migration landed. Schema PRs merge strictly ascending;
   no lower open gap may exist.
4. CI passes the base migration set from `git merge-base origin/main HEAD` into
   `laddercheck.Check(dir, ladder, baseSet)`. The check rejects a newly introduced
   number `<= max(baseSet)`, a number other than `max(baseSet)+1`, an unregistered
   file, or a ladder row without the same-PR SQL file. Parallel schema PRs may be
   intentionally RED until the first lands; contributors rebase and renumber rather
   than weakening the check. The historical sanctioned duplicates remain read-only
   exceptions.

Before migration:

- take and verify a database backup;
- record baseline counts for epics, open epic branches/PRs, review jobs, attention,
  seats/accounts, and active sessions;
- run integrity checks and capture the current dashboard/digest sequence;
- inventory current Interactor/Orchestrator/operational/build/review session routes;
- record the installed actor prompt/runbook versions and seed the initial §18
  protocol-bundle compatibility map.

Migration order:

1. create the default project required by Phase-0 identity and attach current
   repositories;
2. add tables/indexes and required scalar columns with SQLite-safe universal
   defaults, for example `project_id TEXT NOT NULL DEFAULT 'default'`; add values that
   need per-row derivation to a new mapping/table or as nullable only while the same
   forward migration backfills and validates them;
3. backfill `epics.slug` from the legacy ID, assign `legacy:<id>` admission keys, and
   create `UNIQUE(project_id, slug)` plus admission-key uniqueness. Existing epic IDs
   remain opaque legacy identities; new admissions generate ULIDs;
4. backfill every epic with one delivery/review obligation based on the epic record,
   not a repository-wide PR scan;
5. seed recorded branch, builder family, and `builder_affinity_state` from the epic
   row without rewriting `epics.state`;
6. reconcile the exact branch to an artifact when one exists;
7. classify builder-active deliveries as `building`, builder-terminal-but-unmerged
   deliveries as parked affinity plus the fact-derived delivery state, merged
   deliveries as `merged|cleanup_pending`, and abandoned deliveries as `abandoned`;
8. seed the global control-event sequence above the legacy Unix-millisecond digest;
9. validate uniqueness, foreign keys, delivery/review/action bindings, and that the
   existing `ListEpicRunsForRepo`/`EpicForHeadSHA` detection set is unchanged.

SQLite has no `ALTER COLUMN ... SET NOT NULL`. When a safe universal value exists,
use repository house style: `ADD COLUMN ... NOT NULL DEFAULT ...`. If a required
column genuinely has no safe default, the migration must spell out SQLite's forward-
only table-rebuild procedure: backup, maintenance lock, create replacement table,
copy/transform, recreate indexes/triggers, swap, restore/check foreign keys and WAL,
run `foreign_key_check` and integrity validation. It is never described as a simple
post-backfill promotion.

Backfill never adopts an arbitrary PR. If a recorded epic branch has no PR, the
delivery remains `awaiting_artifact`. If it has two plausible PRs, the delivery is
held with an ambiguity alert. If an old epic is terminal in the session table but its
PR is green/unreviewed, it becomes parked/awaiting-review rather than Completed.

The old issue/PR adoption code may remain temporarily for compatibility tests and
legacy data, but while it runs independently it must skip owned epic branches. The v2
materializer absorbs/supersedes any pre-existing adopted job for the same PR. Remove
the legacy path only after no production records depend on it.

### 13.2 Shadow modes

Run three independent shadows before actuation:

1. **Delivery shadow:** exact-branch artifact joins, computed states, missing-handoff
   candidates, and proposed actions are recorded/read-only.
2. **Capacity shadow:** compare current route choices with fail-closed live/identity-
   bound choices; enumerate which seats would become held and why; prove configured
   reserves cannot silently reduce a required pool to zero. Candidate generations use
   separate tables and cannot mutate active `account_windows`.
3. **Fairness shadow:** compute project dispatch order and starvation metrics without
   changing the single/default-project scheduler.

Shadow mismatches appear in the dashboard/audit and are reviewed before enablement.
Shadow mode must not silently mutate GitHub, tmux, leases, or decisions.

### 13.3 Canary sequence

1. Enable the P0.I schema/read models and new dashboard states globally with
   `epic_review_handoff_v2=false`.
2. Run delivery shadow, adoption/stuck-loop collision tests, and exact crash
   failpoints; then enable I.4 review materialization/recovery for one canary while
   `capacity_routing_v2=false` and full contract compilation remains optional.
3. Inject the production crash, poison-fact, reviewer-death, and external-deadman
   tests in the canary.
4. Deploy/health-check the live collector on **every** intended host, run capacity
   shadow including reserve/pool cardinality, then enable fail-closed routing.
5. Enable actor-contract handshake in observe-only mode, then refuse new work to a
   canary actor with an incompatible bundle while compatible actors continue.
6. Enable fenced relaunch/rejection, then merge/cleanup actuation.
7. Make the dashboard decision inbox read/write for the canary project.
8. Make dashboard conversation and automatic work-intent promotion the canary
   Interactor's normal surface.
9. Backfill and enable a second project; run fairness and fault-isolation drills.
10. Expand only while recovery, duplicate-effect, stale-capacity, and decision-ack
   metrics remain within objectives.

### 13.4 Pause and rollback posture

The primary kill switch pauses **project-out actuation** while continuing:

- GitHub/CI/capacity/session reconcile-in;
- durable state and audit writes;
- stall detection and alerts;
- dashboard/read APIs;
- lease expiry/fencing needed to prevent zombies.

This prevents a bad rollout from becoming invisible. Rollback does not drop new
tables or erase obligations. An earlier binary must either tolerate additive schema or
be blocked by a schema-version readiness check. Any manual recovery is expressed as a
typed audited transition/action, not direct ad hoc SQL in the normal runbook.

Before migrations, reconciliation, or network listeners, `serve` acquires an OS
exclusive advisory lock at `control_plane_lock_path` and holds it for process life. A
second process exits non-ready; the pidfile remains signaling metadata only. Legacy
and v2 outboxes claim work with one compare-and-swap transaction that writes claim
owner, epoch, and lease deadline before external effects. No drain retains the old
read-then-send assumption.

### 13.5 Metrics

Use low-cardinality project/repo/state/kind labels; epic/action IDs belong in logs and
traces, not metric labels.

Pipeline metrics:

```text
flowbee_epic_deliveries{project,repo,state}
flowbee_oldest_delivery_state_age_seconds{project,repo,state}
flowbee_review_handoff_recoveries_total{project,repo}
flowbee_review_handoff_stall_age_seconds{project,repo}
flowbee_review_queue_age_seconds{project,repo}
flowbee_review_active_leases{project,repo}
flowbee_epic_actions{project,kind,state}
flowbee_epic_action_oldest_pending_seconds{project,kind}
flowbee_epic_pipeline_transitions_total{from,to}
flowbee_epic_cleanup_pending_seconds{project,repo}
flowbee_delivery_stall_age_seconds{project,repo,state,reason}
flowbee_reconciler_heartbeat_age_seconds{loop}
flowbee_reconciler_panics_total{loop}
flowbee_alert_outbox_pending{tier}
```

Capacity metrics:

```text
flowbee_capacity_observation_age_seconds{provider,trust,source}
flowbee_capacity_probe_attempts_total{provider,outcome}
flowbee_capacity_probe_backoff_seconds{provider}
flowbee_capacity_routable_seats{provider}
flowbee_capacity_held_seats{provider,reason}
flowbee_capacity_identity_drift_total{provider}
flowbee_capacity_generation{state}
flowbee_capacity_reducer_conflicts_total{provider}
flowbee_capacity_pool_exhausted{project,pool}
```

Human/project metrics:

```text
flowbee_decision_requests{project,kind,state,priority}
flowbee_decision_oldest_open_seconds{project,kind}
flowbee_decision_ack_pending_seconds{project,hop}
flowbee_conversation_delivery_pending_seconds{project}
flowbee_work_intents{project,state}
flowbee_work_intent_promotion_age_seconds{project,hop}
flowbee_actor_contract_holds{role,reason}
flowbee_project_scheduler_service_total{project,pool}
flowbee_project_oldest_eligible_seconds{project,pool}
flowbee_project_breakers{project,scope,state}
```

### 13.6 Logs, traces, and audit

Every reconciliation/action log carries:

- trace/correlation ID;
- project, epic, delivery, artifact version, and action/review round as applicable;
- prior/new state version;
- fact source and observation watermark;
- actor/session/lease/action epoch;
- idempotency/dedup key;
- outcome (`noop|applied|recovered|fenced|held|escalated`) and bounded reason.

Sensitive credentials, raw tokens, full transcripts, and unredacted provider identity
material are never logged. Conversation/evidence bodies are referenced by immutable
artifact ID/hash in operational logs.

Audit queries must answer:

- why was this epic admitted and to which project/repo?
- which human request/work-intent version produced it, and did every automatic
  promotion hop acknowledge?
- why did this seat/account route or hold?
- when did the artifact first become review-eligible?
- why was review not active, and what recovered it?
- who/what reviewed which exact head?
- who answered/approved which exact version?
- why did Flowbee merge, reject, pause, or clean up?

### 13.7 Alerts and objectives

Alert conditions include:

- admitted/building/awaiting-artifact/awaiting-CI state past its registered fact
  progress deadline, including CI red and infra-red through their named kinds;
- review-eligible obligation with no execution/action past the handoff threshold;
- queued review with no eligible independent reviewer past its separate capacity
  threshold;
- active review/build/action lease with expired heartbeat;
- action dead letter or acknowledgement timeout;
- artifact ambiguity, identity drift, or branch/session/worktree mismatch;
- no routable seat for sustained eligible work;
- reconciler heartbeat stale, poison row quarantined, external dead-man stale, or
  immediate/dead-letter alert outbox undrained;
- collector/provider breaker, stale entire provider fleet, or capacity generation
  publication failure;
- open human decision/ack chain past its due window;
- defined work intent idle at an Interactor/Orchestrator/admission hop past its route
  deadline, or an actor missing a compatible contract acknowledgement;
- project starvation-bound or breaker-isolation violation;
- merged delivery with cleanup/post-verify overdue.

Initial service objectives:

- missing review handoff recovered within
  `review_handoff_stall + 2 × reconcile_interval` after the control plane is live;
- zero duplicate live reviews, merge actions, or terminal sends for one idempotency key;
- zero merges authorized by a stale verdict/artifact hash;
- capacity observations age out of routing no later than configured max age;
- continuously eligible project work receives service within its starvation bound;
- all open human requests and downstream acknowledgement stalls are visible on the
  dashboard within one digest refresh;
- every continuously eligible work intent is promoted or visibly held within its
  route deadline, without a human reminder.

### 13.8 Runbooks and drills

Ship runbooks for:

- review dispatch stalled/recovered;
- artifact never produced, CI never green/red/infra-red, and builder launch stalled;
- no independent reviewer capacity;
- zero routable build/review/operations pool and prelaunch live-recheck held;
- capacity collector/provider throttled or identity drift;
- project/repository breaker opened;
- action `delivering` after process crash;
- ambiguous or replaced epic PR;
- parked builder resume failure;
- authoritative remote session/proxy mismatch, seat channel unreachable, and audited
  delivery re-home;
- reconciler heartbeat/dead-man/push-webhook failure and poison-row quarantine;
- merge succeeded/cleanup incomplete;
- Interactor/Orchestrator replacement with pending decisions/messages;
- work-intent promotion stalled before delivery, during Orchestrator preparation, or
  after uncertain epic admission acknowledgement;
- actor contract mismatch or missing recovery-contract acknowledgement;
- database restore and startup reconciliation.

Quarterly or pre-release drills should kill each process/session at the crash points in
§12.2 and verify convergence from the durable store.

## 14. Acceptance criteria

### 14.1 Phase 0 acceptance

- Epic admission durably creates the delivery and review obligation before build
  launch.
- Branches/PRs are associated only as output artifacts of that admitted epic.
- The exact production interruption is reproduced and automatically recovered by a
  new process.
- Recovery is idempotent and emits one durable alert/audit record.
- `built · awaiting review dispatch` is a distinct dashboard/API state.
- A queued-but-unclaimed review is treated as capacity wait, not redispatched.
- Builder worktree/branch/identity/scope affinity remains parked through CI/review;
  its pane is torn down, and rejection creates a fenced relaunch after capacity is
  reacquired.
- Review identity/family differs from the builder and verdicts bind to current SHAs.
- Admission refuses a build family without a distinct routable review family.
- CI, rejection, rework, conflict, merge, and cleanup loops self-heal across restart.
- Every non-terminal delivery/CI state has a due clock and next action or durable
  visible hold; awaiting artifact/CI cannot stall silently.
- Arbitrary PRs/issues are never adopted by v2.
- Remote Codex/Grok capacity is live, identity-bound, historically persisted, and
  fail-closed when stale/unverified.
- Shared-account reduction is deterministic and route-time freshness is effective.
- Builder/reviewer/operations use one fail-closed route gate, and zero routable pool
  capacity produces a durable pushed alert.
- Exactly one control-plane writer is OS-lock enforced; every outbox effect is
  atomic-claim/epoch fenced.
- Reconciler panic/heartbeat failure and pre-readiness crashloops are detected by
  supervised loops and durable project-bound watchdog state; every retained incident
  is projected to the exact project Interactor when Flowbee ingress is reachable.

### 14.2 Phase 1 acceptance

- The Flowbee dashboard is the normal human home screen.
- Routine focused human↔Interactor conversation works in the project workspace with
  durable messages and streamed responses, without tmux.
- Questions, plan reviews, design reviews, authorizations, and exceptions are typed,
  prioritized, and visible in one Needs You inbox.
- Human actions bind to the exact artifact/version/hash shown.
- Approve/request-changes/answer/defer/deny actions are authorized, idempotent, and
  audited.
- Conversation framing cannot secretly satisfy a typed gate.
- A sufficiently defined request with satisfied gates automatically becomes a durable
  Orchestrator work item and admitted epic; the human never has to say "push this to
  Flowbee."
- The response and acknowledgements flow durably through
  Interactor→Orchestrator→Flowbee.
- Replacement/compaction of any agent loses neither conversation nor decision state.
- Every actor incarnation automatically loads and acknowledges its current
  role/recovery contract before work delivery.

### 14.3 Phase 2 acceptance

- One global portfolio manages many projects through one logical Flowbee authority.
- Each project has exactly one logical Interactor and one paired Orchestrator route.
- Every epic belongs to exactly one project and declares its repository set plus one
  delivery repository under the initial policy.
- Project and repository are independently visible/queryable/policy-bearing.
- `project_id` scopes durable state, actions, decisions, attention, tmux targets,
  artifacts, costs, and audit.
- Shared build/review capacity is weighted-fair and cannot starve a continuously
  eligible project past the configured bound.
- Project/repository faults are isolated; provider-global faults are scoped only to
  dependent work.
- Killing the process/operational sessions and restarting produces one coherent,
  duplicate-free authority.

## 15. Explicit non-goals

This plan does **not**:

- turn Flowbee into an issue bot or generic PR reviewer;
- auto-adopt a PR because it has a label, closes an issue, is CI-green, or was authored
  by a known account;
- support multiple delivery PRs/repositories per epic in the initial v2 policy;
- infer approval or authorization from conversational prose;
- require the human to operate the Interactor→Orchestrator→Flowbee promotion path;
- treat GitHub reviewer assignment as proof of a live Flowbee review lease;
- let an LLM mutate the deterministic state machine or bypass store invariants;
- use tmux pane scrollback as a durable queue or trusted instruction source;
- fall back to same-family self-review when independent capacity is absent;
- tear down the builder worktree, branch affinity, or scope reservation at the first
  "done" claim (the live pane is intentionally torn down when parking; rework uses
  a fenced relaunch);
- release scope protection while the delivery is merely parked for review;
- guess that a reset occurred because a clock passed a reset timestamp;
- route from cache-only, display-only, stale, malformed, or identity-mismatched usage;
- copy provider credentials into Flowbee's database or send them to workers;
- automatically log into, rotate, or rebind a provider account without explicit
  provider-supported and authorized policy;
- require distributed multi-writer consensus in the first implementation. Durable
  single-writer restart safety is required; replicated authority is a later storage
  evolution behind the same contract;
- replace repository-specific GitHub/CI/merge policy with a project-global shortcut;
- make terminal access disappear. Tmux remains a supported debug/recovery fallback.

## 16. Implementation surface and likely file map

This is a planning map, not permission to edit all files in one branch. Stages should
remain reviewable and avoid unrelated Stream Deck/add-on surfaces.

| area | existing/new likely files | responsibility |
| --- | --- | --- |
| migration allocation | `internal/store/migrations/LADDER.md`, reserved new migrations | additive schema, indexes, backfill shape |
| epic admission/session | `internal/store/epicrun.go`, `cmd/flowbee/epic.go` | atomic delivery obligation, parked affinity, compute-vs-scope reservation |
| delivery/artifacts | new `internal/store/epicdelivery.go`, `epicartifact.go` | state/version CAS, fact projection, read model |
| durable actions | new `internal/store/epicaction.go`, executor package | transactional outbox, claims, retries, receipts, ack |
| epic review execution | `internal/store/flow.go`, `queries.go`, `internal/worker/review.go`, `internal/api/server.go` | native epic link, anti-affinity lease, verdict integration |
| GitHub observation | `internal/github/github.go`, `internal/reconcile/reconcile.go` | head-ref facts, exact owned branch association, CI/merge facts |
| timers/alerts | `internal/store/timers.go`, `internal/alarm/poller.go`, `internal/attention/attention.go`, `internal/store/attention.go` | handoff backstop, no-reviewer capacity wait, durable alert |
| tmux observation/actuation | `internal/tmuxio`, `internal/epicsupervisor`, `tmux-send` action adapter | observer facts from the remote `seat.Box` session, separate status-0–6 actuation receipts, dual local-proxy/remote cleanup |
| capacity probes | `internal/acctprobe/*`, `cmd/flowbee/seat.go`, `cmd/flowbee/supervise.go` | live remote collector, protocol/identity checks, reducer/backoff |
| capacity store | `internal/store/capacity*.go`, new observation/history store | atomic generation, last-good projection, effective freshness |
| projects/scheduler | new `internal/store/project*.go`, scheduler pure core, serve wiring | project model, routes, weighted fairness, breakers |
| decisions/conversation | new pure/store/API packages | typed requests/responses, durable messages, work intents, automatic promotion, ack chain |
| actor contracts/recovery | new `protocol/flowbee/v2/*`, generated `docs/runbooks/actors/*` + `docs/runbooks/recovery/*`, `flows/identities/*`, registry/launcher/doctor checks | role standing orders, automatic prompt injection, allowed recovery, escalation, version/hash handshake |
| dashboard/read APIs | `internal/store/epicboard.go`, `internal/web/handlers.go`, templates/assets, API summary/SSE | portfolio, workspace, Needs You, delivery/capacity truth |
| configuration | `internal/config/config.go`, `docs/config.md` | timings, flags, role pools, project/fairness/capacity policy |
| runtime wiring | `cmd/flowbee/serve.go`, `up.go`, `cmd/flowbee/watchdog.go` | startup lock/leader incarnation, supervised loops, heartbeat/watchdog, external dead-man and push/dead-letter publishers |
| tests | package tests plus `test/acceptance` | crash matrix, incident E2E, fairness, auth, UI, migration |
| docs/runbooks | this doc, `docs/architecture-overview.md`, `docs/pipeline.md`, `docs/glossary.md`, `docs/operating.md`, `docs/troubleshooting.md`, setup/AGENTS guidance | replace legacy issue-lane vocabulary at enablement; publish operator model, actor routes, rollout, and failures |

Suggested ownership split after the schema/transition contract lands:

- workstream A: capacity collector/reducer/history/routing;
- workstream B: delivery/artifact/review handoff and reconciler;
- workstream C: action outbox, `tmux-send` actuator, observer facts, rejection/cleanup;
- workstream D: dashboard/decisions/conversation, work-intent promotion, and
  actor-facing contract projection;
- workstream E: projects/fair scheduler/fault isolation.

The migration and store transition contract must land before parallel workstreams
touch projections. One owner integrates runtime wiring and acceptance tests to avoid
multiple branches independently inventing state edges.

## 17. Adversarial review checklist

The current Flowbee agent should challenge this plan before implementation. Approval
requires concrete answers to each item.

### 17.1 Ownership and state

- Can any issue, label, fork, arbitrary branch, or PR become owned without an admitted
  epic row? If yes, the design is wrong.
- Is the review obligation committed in the same transaction as admission?
- Can projection drift hide a missing handoff from the fact-based reconciler?
- Can a queued/leased/delivering review be mistaken for a missing handoff and
  duplicated?
- Does builder parking preserve worktree/branch/scope affinity while tearing down
  both panes and freeing only active compute?
- Is every non-terminal state reconstructible after killing all agent sessions?

### 17.2 Idempotency and fencing

- What exact unique key prevents two review tasks, terminal sends, merge calls, and
  cleanup actions?
- What happens if the process dies after the external effect but before local commit?
- Does every delayed actor/result carry a lease/action epoch and artifact version?
- Can a stale reviewer, builder, decision response, or merge action advance current
  state?
- Are alert and recovery atomic or backed by a durable pending bit?

### 17.3 Review and merge safety

- Is reviewer independence checked from durable actual identity/family, not configured
  intent?
- Does no reviewer capacity remain visible without relaxing anti-affinity?
- Does every head/base move supersede the verdict and fence the lease?
- Does rejection return to the same builder affinity and require new CI/review?
- Can conflict resolution or post-review changes bypass independent re-review?
- Is `complete` impossible before reconciled merge and cleanup/post-verify?

### 17.4 Capacity correctness

- Are remote Codex/Grok readings live and sanitized on-host?
- Does Codex wait for initialize response and verify `account/read` identity?
- Can stale/display-only/cache/clock-skew/identity-drift data ever route?
- Is freshness evaluated from current time even when the ticker is dead?
- Is same account on multiple hosts coalesced, while duplicate home/drift is held?
- Is account reduction deterministic under every seat order and partial failure?
- Can a lower-trust normal result erase a live critical result?
- Are retry/backoff and reset semantics restart-safe and provider-correct?
- Can a failed window write leave a ready seat in a partial generation?

### 17.5 Human authority and projects

- Can chat prose accidentally authorize an action, or must it become a typed decision?
- Is every response bound to the exact artifact version/hash the human saw?
- Does a defined work intent resume automatically after its real gates pass, with no
  second human "go" or Flowbee-specific instruction?
- Can Interactor/Orchestrator restart or an uncertain admission acknowledgement create
  a duplicate epic or lose the intent?
- Are the Human→Interactor→Orchestrator→Flowbee acknowledgements durable and visible?
- Can any project credential/session/action address another project?
- Does project fairness have a measurable starvation bound under sustained load?
- Can one project/repository/provider fault open a broader breaker than warranted?
- Is one logical authority recoverable without depending on one Grok/tmux transcript?

### 17.6 Operational proof

- Does the exact #4950/#4951-style interruption test fail before the implementation
  and pass after it?
- Are restart, duplicate delivery, stale lease, alert-loss, identity-swap, and
  partial-capacity-generation failpoints exercised with real SQLite transactions?
- Can the human see the missing handoff, recovery, current reviewer, and capacity
  reason from the dashboard without asking an agent?
- Is there a safe actuation pause that leaves reconcile-in and visibility running?
- Are backfill and rollback non-destructive, with no arbitrary PR adoption?
- Can every fresh actor incarnation bootstrap its role, current obligations, legal
  recovery actions, and escalation route without an old transcript or human reminder?
- Does CI fail when actor role cards, recovery codes, schemas, dashboard labels, and
  executable transitions drift apart?

Only after this checklist is answered should implementation begin. Any proposed
simplification must show which invariant it preserves and which failure test proves
it; conversational confidence is not evidence.

## 18. Actor operating and recovery contract

Durable state is useful only if every actor can discover its current obligations,
understand its authority, and resume safely without transcript memory. Flowbee
therefore ships one versioned operating contract and derives bounded, role-specific
runbooks from it. The full architecture plan is never used as a runtime prompt.

### 18.1 One canonical contract, many bounded views

The documentation suite has four layers:

1. This document explains architecture, rationale, invariants, rollout, and
   adversarial review. It is for humans designing and reviewing the system, not for
   runtime injection.
2. `protocol/flowbee/v2/actor-protocol.yaml` is the normative machine-readable
   contract. It defines roles, capabilities, assignment envelopes, acknowledgement
   states, recovery codes, escalation routes, forbidden operations, and protocol
   compatibility.
3. `protocol/flowbee/v2/schemas/` contains the referenced message, assignment,
   result, decision, work-intent, recovery, and collector schemas. Every schema has a
   stable ID and content hash.
4. Generated role cards and recovery runbooks live under `docs/runbooks/actors/` and
   `docs/runbooks/recovery/`. Dashboard help, API descriptions, prompt fragments, and
   recovery-code labels are generated from the same contract.

Generated files are checked in for review but are not hand-edited. A contract
compiler produces:

- one small common safety kernel;
- one bounded role card per actor;
- one runbook per recovery code;
- assignment/result JSON schemas and fixtures;
- dashboard recovery labels and help links;
- protocol/version compatibility data used by `flowbee doctor`.

An agent activation receives only the common safety kernel, its role card, and the
current assignment or recovery envelope. Large plans, evidence, diffs, and transcripts
are immutable references fetched only when needed. CI enforces size budgets for the
common kernel and each role card so this design document can never silently become an
agent's operating prompt.

The common safety kernel contains only rules shared by every actor:

- the database is the workflow source of truth;
- transcript and pane contents are observations, not durable intent;
- act only under a current project-scoped assignment, capability, and epoch;
- bind every judgment or effect to the supplied artifact version/hash;
- retry with the existing idempotency key; never invent a replacement identity;
- recovery means completing or verifying existing intent, not broadening it;
- on ambiguity, persist a hold or escalation rather than guess.

### 18.2 Standing work-intent promotion contract

The human must never have to say "push this through Flowbee" after asking for ordinary
product work.

An ordinary work request in the dashboard conversation becomes the durable,
versioned `work_intent` from §5.6 and §6.7. The Interactor must receive its persistence
acknowledgement before reporting that it accepted the request. The automatic route is:

1. The Interactor captures and refines the work intent.
2. If definition, plan, design, authorization, or product answers are required, the
   Interactor creates the corresponding typed decision request.
3. Once the configured definition and decision gates are satisfied, the control
   plane automatically enqueues the current intent version to the project's paired
   Orchestrator.
4. The Orchestrator claims it, materializes one epic contract, and submits it with
   `(project_id, work_intent_id, intent_version)` as the idempotency spine.
5. Flowbee admits exactly one epic for that key and durably acknowledges the epic ID.
6. The dashboard advances from defining or waiting to Orchestrator delivery,
   submitting, admitted, and then the normal delivery states.

A policy-required approval remains explicit and version-bound. Casual chat cannot
authorize a sensitive gate. However, after the required answer or approval is
recorded, there is no second "go," "submit," or "push this through Flowbee"
confirmation. The pipeline resumes automatically.

Interactor and Orchestrator startup recovery drains pending work-intent and
acknowledgement states before accepting new conversational work. The promotion
reconciler from §8.8 repairs:

- a ready intent with no current Orchestrator action;
- an Orchestrator action with no live claim;
- an epic contract prepared but not submitted;
- an admission submission with an uncertain outcome;
- an admitted epic whose acknowledgement was not delivered upstream.

For an uncertain admission, recovery first queries Flowbee by the original
idempotency key. It resubmits only if no epic exists. A stalled route becomes a
distinct dashboard state and durable alert; it is never left as an unobservable
promise in an Interactor transcript.

### 18.3 Actor responsibility and recovery matrix

| actor | authority and required output | autonomous recovery | must escalate | forbidden |
| --- | --- | --- | --- | --- |
| Human | State product intent; answer typed questions; approve exact plans/designs; authorize exceptional actions. | Retry an idempotent dashboard submission, amend/cancel an intent, or answer the current Needs You item. Mechanical recovery is not a human responsibility. | Product choices, sensitive authorization, destructive exceptions, or irreducible ambiguity. | Dispatching reviews, editing pipeline state, repairing tmux manually as the normal path, or being asked for an extra "go" after required gates are satisfied. |
| Dashboard client | Render durable read models; submit messages, typed decisions, and authorized recovery commands with idempotency/version fields. | Refetch after SSE gaps, retry the same request key, and show pending acknowledgement chains accurately. | Authentication failure, stale subject version, or an operation requiring broader authority. | Computing pipeline truth locally, treating SSE as a ledger, converting chat prose into approval, or presenting an unacknowledged effect as complete. |
| Interactor | Own the project conversation; persist ordinary requests as work intents; frame typed decisions; route ready intent to the paired Orchestrator. | Rehydrate threads, pending intents, decisions, and acknowledgement chains; replay the same durable route action. | Missing product information or human authority through a typed decision request. | Dispatching build/review/merge work, inferring approval from prose, remembering work only in transcript, or asking the human to push a ready intent onward. |
| Orchestrator | Own project priorities, dependencies, definition quality, issue grouping, and epic materialization. | Drain ready intents; reconstruct an epic from immutable intent artifacts; query admission by idempotency key before retrying. | Product ambiguity to the Interactor; invalid repo/project policy to the typed decision path. | Owning post-admission continuity, manually dispatching review, adopting arbitrary PRs/issues, or bypassing an unsatisfied human gate. |
| Deterministic Flowbee core | Own durable state, legal transitions, leases, gates, scheduling, reconciliation, alerts, and external-effect outboxes. | Reap stale leases; reconcile facts; repair missing actions; retry or verify uncertain effects; resume all non-terminal work on startup. | Bounded operational judgment to the operational agent; product judgment through the Orchestrator route; human authority through typed escalation. | Depending on agent/session memory, accepting agent prose as GitHub fact, exposing generic "set state," or silently failing open. |
| Operational agent (Grok by default) | Perform bounded operational supervision under a durable action: diagnose build logistics, apply an authorized minor repair, and report evidence. | Query its recovery view; resume a live fenced action; verify prior receipts before retry; relinquish stale work. | Product/logistics judgment to the Orchestrator; human authority through the Interactor; invariant or security failure to durable attention. | Direct database transitions, product decisions, artifact adoption, blind terminal resend, or acting as a reviewer without a separate reviewer lease. |
| Builder (Codex by default) | Implement one bounded epic on its registered branch/worktree and report structured progress/evidence. | Reattach or relaunch the same affinity under a new incarnation; verify current assignment/head; resume rework after capacity is reacquired. | Scope ambiguity, product conflict, unsafe destructive change, or inability to satisfy acceptance criteria. | Choosing the next pipeline state, merging, reviewing its own work, touching another epic's branch/worktree, or making GitHub calls with worker credentials. |
| Reviewer (Grok by default) | Independently judge the exact assigned artifact and return a structured SHA-bound verdict/evidence. | Resume only while its lease, epoch, artifact version, and SHAs remain current; otherwise discard local work and reacquire. | Missing evidence, unsafe change, or a product question that cannot be decided from the epic contract. | Reviewing a different head, same-family/self-review fallback, modifying the delivery, merging, or returning a verdict after fencing. |
| Tmux DriverPort | Own exact session/process/provider identities, watches, observation archive, leases, draft protection, closed-template input/dialog handling, grants, verified receipts, and control APIs; it never chooses Flowbee workflow transitions. | Reconcile its own transport uncertainty from stable identities, pane locks, receipts, and observation evidence; return typed facts to Flowbee. | Session ambiguity, target mismatch, unreachable host, grant denial, or unprovable transport outcome. | Product routing, GitHub/CI truth, merge/review decisions, arbitrary pane targeting, or treating transport as stage completion. |
| Capacity collector | Perform read-only on-host provider probes and return sanitized, identity-bound observations. | Honor durable backoff, retry live collection, preserve last-good history for display, and report identity drift distinctly. | Authentication/identity drift, provider schema change, or repeated live-read failure. | Choosing routes, exporting credentials, promoting cache/display data to routable, relabeling provider windows, or inferring reset without a fresh observation. |

A model family does not imply authority. A Grok session acting as operational
supervisor has no reviewer capability unless it separately registers, receives, and
claims a reviewer assignment. Likewise, replacing an agent incarnation does not
inherit an old epoch merely because its tmux name is similar.

### 18.4 Startup and assignment protocol

Every process or agent activation performs a version handshake before receiving work.
It presents:

```text
actor identity and role
project/host/session scope
session incarnation
supported protocol major/minor versions
supported capability and schema IDs/hashes
compiled role-card bundle hash
```

Flowbee returns the negotiated protocol version, granted scope, recovery cursor, and
current server bundle hash. Compatibility rules are fail-closed:

- the protocol major must match;
- assignment schemas must have an exact supported ID/hash;
- optional minor capabilities are explicitly negotiated;
- an incompatible actor receives no new lease and appears as
  `protocol_incompatible`;
- an already-running assignment may drain only while its exact contract remains
  supported; otherwise it is parked and fenced.

After negotiation, every actor requests its recovery snapshot before accepting new
work. An assignment envelope contains only:

```text
protocol and schema version/hash
assignment/action/lease ID and epoch
project, epic, role, and target incarnation
desired bounded outcome
current artifact version/hash and SHAs when relevant
allowed result/command kinds
immutable evidence references
acknowledgement and lease deadlines
recovery code/endpoint and escalation route
idempotency key
```

The actor explicitly accepts or rejects the envelope with a typed reason. It never
infers an assignment from a pane, PR, issue, branch name, or prior conversation.
Tmux Driver treats a cleared/compacted/replaced agent context as a new logical
incarnation when contract retention cannot be proven; Flowbee re-sends the bounded
bootstrap bundle and requires a fresh acknowledgement before the next action. Every
assignment repeats the contract/schema IDs and recovery endpoint, so even a healthy
long-lived actor does not depend on remembering an old human explanation.

Control-plane readiness has its own startup order:

1. validate schema migration and protocol-bundle compatibility;
2. establish the process/leader incarnation;
3. expire or fence stale leases/actions;
4. reconcile external facts;
5. rebuild projections and route/hold explanations;
6. drain work-intent, decision, epic-action, alert, and notification outboxes;
7. run missing-next-action reconcilers;
8. become ready for new admissions and leases.

This makes startup recovery part of normal operation rather than an exceptional
operator procedure.

### 18.5 Machine-readable recovery views

The private actor API exposes a scoped bootstrap/recovery model, conceptually:

```text
GET /v1/internal/actors/me/bootstrap
GET /v1/internal/actors/me/recovery?cursor={cursor}
```

The response contains protocol/bundle identity, actor incarnation, project scope,
current fenced assignments, pending acknowledgements, work-intent routes, durable
holds, deadlines, permitted domain commands, evidence references, and the next
recovery probe. It contains neither raw provider credentials nor unrelated project
state.

Every non-terminal state and durable hold maps to a registered recovery code. Each
recovery-code definition contains:

- invariant and operator-readable meaning;
- authoritative read-model predicate;
- owning actor;
- whether automatic repair is permitted;
- idempotent repair command/action kind;
- required state version, epoch, and artifact binding;
- retry/backoff and maximum automatic-attempt policy;
- escalation target and threshold;
- dashboard severity/copy/help link;
- audit event and required drill/test ID.

Initial codes include at least:

```text
work_intent_capture_unacked
work_intent_promotion_stalled
orchestrator_claim_expired
epic_admission_outcome_uncertain
epic_admission_ack_stalled
review_dispatch_missing
review_capacity_unavailable
review_claim_stalled
action_delivery_uncertain
action_ack_overdue
lease_expired
artifact_advanced
protocol_incompatible
capacity_live_unavailable
capacity_identity_mismatch
merge_outcome_uncertain
cleanup_overdue
decision_ack_overdue
artifact_never_produced
ci_never_green
ci_red_on_epic_pr
ci_infra_incident
builder_launch_stalled
builder_progress_stalled
review_dispatch_flapping
rework_dispatch_stalled
merge_dispatch_stalled
conflict_resolution_stalled
review_verdict_overdue
capacity_pool_exhausted
capacity_recheck_failed
seat_channel_unreachable
reconciler_dead
orchestrator_route_absent
operational_supervisor_absent
builder_host_unreachable
```

Recovery APIs expose domain operations such as "verify or retry action," "re-arm
expired lease," or "acknowledge current intent version." There is no generic state
mutation endpoint. A recovery command is rejected unless the caller's role, project
scope, expected version, epoch, and idempotency key all match.

### 18.6 Escalation contract

Recovery proceeds at the lowest authorized layer:

```text
deterministic retry/reconcile
  → bounded operational-agent recovery
  → project Orchestrator for product/logistics judgment
  → project Interactor for human framing/authority
  → human through the dashboard Needs You inbox
```

Builder, reviewer, driver, and collector failures first return structured facts to
the control plane. They do not independently page the human or choose a different
route.

Every escalation is itself a durable typed record containing recovery code, project,
epic/action, current version/epoch, attempted repairs, evidence references, waiting
actor, requested decision or authorization, severity, and deadline. Pane messages and
chat mentions may wake an actor but are never the escalation record.

A transient condition that self-heals remains visible in audit/history but need not
interrupt the human. Repeated failure, exhausted automatic repair, product ambiguity,
or required authority creates the appropriate Orchestrator/Interactor/Needs You item.
Acknowledgements at every hop remain visible until the originating condition is
resolved.

### 18.7 Universal forbidden recovery shortcuts

No actor, including a human debugging through tmux, may use the normal recovery path
to:

- edit durable state with ad hoc SQL;
- invent a new epic, action, lease, route, or idempotency key to replace an uncertain
  one;
- blindly resend an effect whose prior outcome can be verified;
- act after an epoch, incarnation, artifact version, or subject hash mismatch;
- infer ownership from an arbitrary issue, PR, label, branch, author, or CI result;
- treat transcript/pane text as an approval, verdict, GitHub fact, or durable queue;
- bypass project scope, reviewer independence, capacity trust, CI, merge, or decision
  gates;
- turn a successful transport receipt into workflow completion;
- ask the human to perform a routine mechanical handoff;
- require a second human "go" after the actual configured decision gate has passed.

Exceptional manual recovery uses a typed, least-privilege, expiring authorization and
a domain command that produces normal audit/state transitions. Direct database repair
is break-glass disaster recovery only and requires a backup, maintenance mode, and a
post-repair reconciliation/audit procedure.

### 18.8 Documentation-as-code validation

CI runs the contract compiler in check mode and fails when:

- generated role cards, recovery runbooks, API schemas, dashboard labels, or fixtures
  differ from the canonical contract;
- a role, state, action kind, hold reason, or recovery code lacks an owner, next
  action, escalation route, and dashboard representation;
- an implemented state transition or API command is absent from the contract, or a
  contracted operation has no implementation/test;
- a non-terminal state has neither an automated recovery path nor a durable visible
  hold;
- an external effect lacks an idempotency key, verification method, and fence;
- a role card grants an operation outside the executable authorization matrix;
- a schema changes incompatibly without a protocol-major change and migration note;
- common-kernel or role-card prompt size exceeds its budget;
- generated runbooks contain secrets, mutable external prose, or unbounded evidence;
- links, schema examples, or assignment/result round trips fail validation.

`flowbee doctor` additionally reports the installed protocol version and bundle hash,
checks the database/runtime contract version, and lists incompatible or stale live
actors. The dashboard displays each actor's negotiated version and links every hold
or alert directly to the generated recovery entry.

### 18.9 Recovery drills and acceptance

Each release candidate runs scripted restart drills with a new actor incarnation and
no inherited transcript:

1. Capture an ordinary human work request, kill the Interactor before downstream
   routing, restart it, and prove the same intent reaches the Orchestrator once.
2. Commit a required plan/design approval, kill the acknowledgement route, and prove
   promotion resumes automatically without asking the human for another "go."
3. Kill the Orchestrator before and after epic submission and prove the idempotency
   query returns one admitted epic.
4. Reproduce the build-green/review-undispatched incident and prove startup/periodic
   reconciliation creates one review and one alert.
5. Kill the operational action executor before send, after send, and before receipt
   commit; prove recovery verifies before retrying.
6. Expire builder and reviewer leases; prove stale results are fenced and current
   assignments recover.
7. Interrupt capacity collection and change a provider identity; prove stale/drifted
   seats become visible but non-routable.
8. Start an incompatible actor and prove it receives no lease while compatible
   actors continue.
9. Restart the Flowbee process with pending intent, decision, review, merge, cleanup,
   and notification actions; prove readiness waits for recovery and no effect
   duplicates.

## 19. Adversarial closure matrix (v0.3)

This revision is the response to Fable's 9-lens `NO_GO`. It preserves the durable
admission-time review obligation, five-axis delivery state, SHA/epoch fencing, and
fact-based reconciliation spine. The table is a build gate: each row must have a
named implementation proof before the corresponding flag is enabled. This document
revision itself does not constitute implementation or a new sign-off.

| review blocker | closure in this document | proof required before enablement |
| --- | --- | --- |
| 1. upstream silent seams | every delivery/CI state has a due clock, owner, next action/hold, delivery-wide backstop, and CI-red/infra-red attention kinds | registry property plus artifact/CI/red/infra-red crash tests |
| 2. legacy AdoptSweep collision | owned epic branches are excluded; materialization absorbs/supersedes adopted jobs; epic actors cannot apply legacy labels | pre-CI adoption test with exactly one native review |
| 3. stuck/janitor escalation | `workflow_domain=epic_v2` jobs are fenced from legacy liveness/stuck/janitor transitions; capacity wait remains queued | job/delivery agreement and no-`needs_human` capacity test |
| 4. admission duplicate/lost ack | ULID surrogate, project-scoped slug and admission-key uniqueness, atomic `admitted_epic_id` in `AddEpicRun` | fresh-slug retry returns the original epic |
| 5. moved-head dedup | action keys use immutable head+base SHA; eligibility resets on advancement | two-head test derives two keys and reopens CI/review |
| 6. post-ack duplicate effects | full-history unique action/effect ledger; only pre-effect `cancelled_superseded` rows leave the live index for SHA revisit | acknowledged/dead-lettered insertion fails; H1→H2→H1 revisit has one live action |
| 7. unsafe re-review edges | rebuild/conflict return to CI; only merged reaches cleanup; head advance cancels live effects and returns to CI | state-graph and supersession property tests |
| 8. incomplete ledger/digest | `control_events` has global AUTOINCREMENT `seq`; digest is `MAX(seq)` across state, job, action, capacity, and alert changes; cutover is seeded above Unix-millis | restart/digest test observes pre-stall delivery/action changes |
| 9. dead reconcilers/pull-only alerts | supervised per-item recovery, heartbeat/watchdog, external dead-man, push/dead-letter channel, poison-fact quarantine | poison panic and pre-readiness crash tests page independently |
| 10. overlapping writers/outbox | process-lifetime OS lock/leader incarnation; all outboxes atomic-claim with epoch | two-process writer and duplicate-effect tests |
| 11. observer/actuator mismatch | Tmux Driver is the sole observation and routed-actuation boundary; Flowbee never calls raw tmux or a standalone `tmux-send`. Exact pane/run fencing, grants, idempotency, and uncertain receipts are normative. | routed actuator status matrix and uncertain-send tests |
| 12. zero builder pool invisibility | durable `capacity_pool_exhausted` attention/reconciler/threshold/push path | zero-routable-pool exit test |
| 13. unknown windows/green-by-absence | provider required-window rules, Grok known-zero carveout, strict real-success/all-checks/not-truncated green predicate | missing-window, Grok-zero, and CI predicate tests |
| 14. fail-open reviewer selector | one per-seat freshness/identity/lineage route gate for builder/reviewer/operations; legacy `usage_pct` selector retired/hardened | stale selector and legacy-writer regression tests |
| 15. no distinct reviewer family | admission fails closed without distinct configured reviewer capacity; Grok operations reserve is first-class; Grok/Claude builder migration is explicit | family-capacity admission and operational-reserve tests |
| 16. parked seat oversubscription | park tears down panes; only affinity persists; occupancy is physical; rework is fenced relaunch | parked/relaunch and occupancy tests |
| 17. dual tmux reality | remote `seat.Box`/`tmux has-session` is authoritative; local attach proxy is auxiliary; cleanup handles both | remote launch/cleanup and duplicate-session tests |
| 18. legacy capacity writer | `foldSeatCapacity → UpsertAccountLimits` is disabled under v2 and cannot mutate new generations | flag-on writer regression test |
| 19. rollout ordering | minimal P0.I incident slice is separately gated and shippable before parallel P0.CAP/P0.CONTRACT | stage-gate test and canary checklist |
| 20. `epics.state` merge safety | retain `done/achieved`; add delivery projection/affinity state; enumerate `ListEpicRunsForRepo`, `EpicForHeadSHA`, review lease, attention, and dashboard reads | migration/read-set compatibility test |
| 21. SQLite nullability | use `ADD COLUMN NOT NULL DEFAULT` when safe; defaultless changes use documented 12-step table rebuild | migration compile/restore test |
| 22. migration ladder gaps | next merge-time number is 0032; never backfill 0029/0030; reserve+SQL+LADDER in one PR; strict ascending and `<=max(main)` rejection | laddercheck and merge-order tests |

The SHOULD-FIX items are also normative: the master/attention lease maps to the
operational Grok role, Flowbee is the plane, hold lifecycles are total, clocks use
observed facts, terminal/abandoned rows cannot resurrect, alert draining and
builder-launch/re-home are independent reconcilers, pre-launch failures are durable
holds, freshness is per seat, flapping is capped, and every recovery code traces to
§12.8's failpoint and named test. Fable should re-run the nine lenses against this
matrix and the implementation slices before any production code is started.
10. Attempt every forbidden cross-role command and prove executable authorization
    rejects it.

The actor contract is accepted only when:

- a fresh replacement of every actor can determine its current work and legal next
  action from the recovery API plus its bounded role card;
- no recovery requires access to a prior transcript or a human memory prompt;
- one ordinary human work request automatically reaches Flowbee admission once its
  real definition/decision gates are satisfied;
- every hop and acknowledgement is visible from the dashboard;
- every non-terminal state has exactly one recovery owner and a deterministic next
  action or durable hold;
- incompatible contracts, stale actors, and forbidden operations fail closed;
- all repairs are idempotent, fenced, project-scoped, and audited;
- generated documentation and executable behavior cannot drift without failing CI.
