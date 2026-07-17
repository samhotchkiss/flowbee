# Flowbee epic-lane rework — the committed design + phased plan (Phases 4–9)

**Status:** authoritative. Lands as `docs/design/epic-lane.md` and becomes the committed
phase plan (only `epics/INSTRUCTIONS.md` exists today; the plan was never committed).
Continues the merged epic-lane numbering (Phases 0–2 merged; Phase 3 implemented-unmerged).

**Synthesis basis (judges, unanimous):** the **reliability design is the spine** — it owns the
one load-bearing risky seam (exactly-once-in-practice master-reply delivery into a live pane)
and grounds every posture in mechanisms already proven in the codebase. Onto that spine we graft:
**throughput's cleaner phase-split** (its P4 land-gate → P5 queue → P6 capacity/digest → P7
dashboard, discarding reliability's overloaded single Phase 4), **operator-UX's human override
drawer and its dedicated `supervisors` table kept structurally separate from `goal_sessions`**,
throughput's **anti-collocation launch rule + `flowbee epic plan` placement read-model**, and
UX's **`flowbee master poll` one-call loop**. Every judge-flagged `missing_everywhere` gap is
resolved below in §12 (design answer or consciously-accepted risk), and each is wired to a phase.

---

## 0. Priority order and the central bets

Optimize, in order: **reliability/safety** (no lost epics, no false verified-sends, no injection
surface growth, graceful master death) → **throughput** (saturate capacity without mid-epic
exhaustion) → **operator UX** (one dashboard, human override). The bets:

1. **A durable, epoch-fenced attention queue is the master's memory, not its transcript.** Every
   item is dedup-keyed so a fresh (post-`/clear`) master rediscovers the exact open set
   idempotently. This is the `/loop` context-bloat lesson made structural.
2. **One deterministically-compiled digest answers "is every session on task?" in a single cheap
   call** — never pane-scraping by the master. Drift is caught by deterministic control-plane
   signals; only the *judgment* is leased to a cheap advisor or the master.
3. **Master-authored guidance is a first-party payload delivered exactly-once-in-practice**
   (`state=delivering` + `idempotency_key` + pane re-capture before any re-send), fenced by the
   item's lease epoch, fully ledgered. Pane/scrollback content never becomes keystrokes.
4. **The master is a third supervised-actor kind (`supervisors` table), never a `goal_session`.**
   This is a hard correctness fix: `watchdog.Pass` iterates `ListEnabledGoalSessions` and types
   `/goal resume` into anything it classifies blocked; registering the master there would fire the
   resume machinery into the master's own pane. The master gets an *independent* lightweight
   pane-idle backstop that the auto-resume path never touches (§1.6).
5. **Control-plane sends are built from a per-agent-family verb table**, not hardcoded Codex
   `/goal` strings. Resume/launch keystrokes differ per agent family (Codex `/goal` builtin vs
   Claude Code loop mechanisms); a single verb table keyed on family is the keystroke analogue of
   the migration-numbering discipline (§1.7).

The accepted cost: a fatter operator surface and a handful of new tables. Every phase keeps
`go test ./...` (incl. `test/acceptance`) green and the fleet operational.

---

## 1. Q1 — Master registration & the attention queue

### 1.1 What a master is
A long-lived Claude Code (or Codex) **interactive tmux session in its OWN pane** that registers as
a supervised actor and supplies leased judgment. It is not a worker and **not** a `goal_session`.
It lives in a dedicated `supervisors` table (operator-UX's cleanest separation) so it is never
swept into `watchdog.Pass`. Anti-affinity advisory: a master should be a different model family
from the builder it corrects (enforced only for the single review event, §5b).

### 1.2 Registration & transport — HTTP primary, CLI wrapper
Both, mirroring the worker API (HTTP long-poll) and `flowbee session`/`host` (CLI). Registration
is an **idempotent upsert keyed on a stable `label`** (opposite of `AddGoalSession`/`AddEpicHost`,
which fail loud) — re-registration is *expected* on every `/clear` or restart.

```
POST /v1/masters/register   (auth: worker token)
  { "label":"master-pearl", "kind":"claude", "model_family":"claude",
    "box":"", "tmux_name":"master", "repos":["flowbee"], "agents":["claude","codex"] }
  → 200 { "master_id":"…", "epoch":7, "heartbeat_interval_s":30, "open_items":4 }
```
The upsert **bumps `epoch`**, fencing every lease the prior incarnation held. A brand-new master
and a post-`/clear` master are the same code path: register → read `open_items` → lease → resolve.

`POST /v1/masters/{id}/heartbeat { epoch } → { ok, open_items, revoked }` — `revoked:true` means a
newer registration superseded you; stop and re-register. Heartbeat is also folded into
`flowbee master poll` (§2.2) so the skill never tracks a separate call.

### 1.3 The attention queue — a parallel lighter table, not the jobs engine
**Decision: a dedicated `attention_items` table (migration 0027), NOT `jobs`.** Jobs carry a heavy
PR lifecycle (spec→review→merge, outbox, CI facts, minted verdicts) an ephemeral supervision nudge
does not have and the `Decide` state machine has no transition for "master typed a nudge." A
dedicated table keeps the blast radius small and copies only the two properties that earned their
keep: **epoch fencing** (identical `ErrStaleEpoch` semantics to `internal/lease`) and **ledgering**
(every create/lease/resolve/escalate appends a `ledger.Event`).

**Taxonomy** (`kind`; producer; default priority — lower = more urgent, matching
`0021_priority_lower_is_urgent`; dedup_key; resolution):

| kind | produced by | prio | dedup_key | master action |
|---|---|---|---|---|
| `scope_violation` | drift: epic diff outside scope globs, OR a main-merge landed *inside* an active epic's reserved tree | 5 | `<epic>:scope` | HALT/amend — contract breach (both directions) |
| `launch_failed` | `epic start` rollback path / launching-reaper | 10 | `<epic>:launch_failed` | retry/reassign host or account |
| `blocked_non_resumable` | watchdog `blockInfra`/`SetNeedsOperator` | 10 | `<epic>:blocked` | diagnose; usually operator, master may unblock |
| `auth_dead` | auth-death classifier (§12.4) | 10 | `<epic>:auth_dead` | **human-only re-login** — never auto-resume |
| `master_absent` | reaper: high-pri item unleased > T with no live master | 3 | `master_absent` | operator/push alarm (not master-facing) |
| `wedged_ui` | digest: pane in modal/copy-mode > T | 15 | `<epic>:wedged` | author exact escape key (family verb table) |
| `drift_suspect` | drift detector after advisor says "off" | 15 | `<epic>:drift:<signal>` | correct or `## Amendments` |
| `usage_critical` | capacity monitor: assigned account critical (non-stale) | 15 | `<epic>:usage:<acct>` | ride/pause/reassign decision |
| `needs_input` | digest: pane AWAITING_INPUT | 20 | `<epic>:needs_input` | author reply → verified send |
| `ci_red_on_epic_pr` | CI reconcile on epic head red (real, not flake) | 20 | `<epic>:ci_red:<sha>` | inspect; nudge fix or wait |
| `ci_infra_incident` | CI classifier: red is a suspected infra-flake | 25 | `ci_infra:<repo>` | fleet banner; auto-rerun-once (§12.5) |
| `merge_main_suggested` | main moved adjacent to active scope | 35 | `<epic>:merge_main` | author `## Amendments` merge-of-main |
| `epic_finished` | ingestion: `State: done`/`achieved` | 40 | `<epic>:finished` | verify PR opened+labeled; dismiss (audited, §12.8) |

**Dedup discipline.** A partial UNIQUE index on `dedup_key WHERE state IN ('open','leased')`
enforces one active item per condition *structurally*. Every producer calls `UpsertAttentionItem`:
a re-seen key bumps `last_seen_at`/`occurrences` and refreshes evidence — never a second row. When
the condition clears (pane leaves AWAITING_INPUT, CI green, account drops below critical) the
producer auto-resolves with `resolution='cleared'`. The open set is a pure function of current
reality, not of how many ticks fired — the property that makes a fresh master idempotent.

### 1.4 Digest-then-lease (idempotent for a fresh master)
```
GET  /v1/masters/attention?state=open            # read-only view, no lease
POST /v1/masters/attention/lease { master_id, epoch, max:5, kinds:[…] }
  → { items:[ {id, kind, epic, priority, evidence, dedup_key, item_epoch}, … ], lease_expires_at }
```
Lease marks each row `state=leased, leased_by, item_epoch++, lease_expires_at=now+TTL`, atomic in
one serialized tx, one in-flight item per target session (never two masters driving one pane). A
master that died does **not** remember what it leased: it re-registers (bumps `epoch`, orphaning
old leases), the reaper returns them to `open`, it re-leases from scratch.

### 1.5 Resolution — exactly-once-in-practice, fenced, ledgered, ack-closed
The crux of "exactly-once delivery" and "no false verified-sends." (Reliability's crown jewel,
kept verbatim, extended with UX's tamper-evidence bind and a real send-and-ack loop.)

```
POST /v1/masters/attention/{id}/resolve
  { master_id, epoch, item_epoch, action:"reply",
    payload:"Step 4's fixture path moved; point it at internal/store/testdata/… and re-run.",
    idempotency_key:"<client-generated, stable per intended send>" }
```
Server sequence (serialized store tx up to the send boundary):
1. **Fence.** Reject unless `state=leased AND leased_by=master_id AND item_epoch matches AND
   supervisor.epoch matches` → else `409 fenced`. Same fencing that stops two workers
   double-completing a job.
2. **Validate payload** (injection guard): length cap (4 KB), reject control/escape bytes, reject
   if the target pane's identity changed since lease (epic abandoned/relaunched). The payload is
   **first-party master-authored text** — categorically different from reflected scrollback — and
   is delivered as a **bracketed paste** so no byte is interpreted as a terminal control sequence.
   It is DATA, not a verb; control decisions (resume/launch/bare-Enter) remain literal registered
   verbs from the family verb table (§1.7).
3. **Transition then send.** Atomically set `state=delivering, delivery_key=idempotency_key`, then
   `tmuxio` delivery-verified send into the epic's pane (bracketed paste → separate Enter → settle
   → **exact-last-line match** → copy-mode guard → nudge), recording Strong/Weak/Failed:
   - **Strong** → `state=awaiting_ack` (NOT resolved — see send-and-ack below). Ledger `epic_intervention`.
   - **Weak** (pane changed, not exact-matched) → `state=awaiting_ack, verdict=weak`; next digest re-checks.
   - **Failed** → one retry; still Failed → `state=open, detail=delivery_failed`; persistent → escalate `blocked_non_resumable`.
4. **Send-and-ack (§12.3).** `awaiting_ack` is not success. The next digest tick re-checks the
   target: did `pane_state`/`## Status`/commit advance in response within `T_ack` (default 6m)? If
   yes → `state=resolved, resolution='acked'`. If no → re-open with `detail=steer_not_processed`
   (the steer was absorbed but changed nothing) so a politely-stalling agent cannot mask an
   unremedied drift. Delivery verification proves *submission*; the ack loop proves *processing*.
5. **Ledger.** Every intervention appends a `ledger.Event` (`epic_intervention`) bound to
   `{epic, item, master_id, epoch, payload_hash, pane_hash@send, head_sha, verdict}` — UX's
   tamper-evidence bind, but written to the **ledger** (one source of truth), not a parallel
   `interventions` table (which the judges flagged as a dual-write drift risk). The drawer's
   "intervention history" reads `AllAudit` filtered by epic.

**Crash-window handling (accepted residual risk, bounded).** If the master crashes between a
successful send and the store recording the verdict, the item sits in `state=delivering` with its
`delivery_key`. The reaper, after TTL, does **not** blindly re-send: it **re-captures the target
pane** and asks tmuxio "does the last submitted line already match this payload?" If yes → mark
`awaiting_ack` (idempotent recovery, no second send). If the pane moved on / cannot match → re-open
for a fresh decision. The only way to get a duplicate nudge is a crash in the sub-second window
between send-verified and pane-moved-on, and a duplicated context-aware instruction is low-harm.
True exactly-once over a fire-and-forget terminal is impossible; we do not pretend otherwise.

Other actions: `dismiss` (resolve, no send — e.g. `epic_finished`, but audited, §12.8),
`escalate` (route to operator `NeedsHuman` + optional push), `amend` (a verified send the master
authors as an `## Amendments` entry — the sanctioned, contract-recognized way to authorize a
merge-of-main or scope change; INSTRUCTIONS.md line 10 already recognizes `## Amendments`).

### 1.6 No master / master death — two independent liveness sources
- **No master, items accumulating.** Items are durable; they wait. A reaper raises `master_absent`
  (→ existing `NeedsHuman`/`needs_operator` operator sink + optional `PushNotification`) when an item
  goes unleased past its kind's escalation window with no live heartbeat. (Original draft said a flat
  "priority ≤ 15 / T_absent 10m" rule — SUPERSEDED by §15.4's per-kind tiers, which the implementation
  follows: human-first kinds page immediately; master-first kinds page only after their per-kind window;
  never-page kinds (`epic_finished`, `merge_main_suggested`, `ci_infra_incident`) do not page. With
  multiple masters, liveness = max(last_heartbeat) over non-stale supervisors.)
- **Heartbeat source.** Heartbeat older than `3× interval` → `state=stale`; leases reaped to `open`.
- **Pane-idle backstop (watches the watcher), with the explicit carve-out.** The master's own pane
  is registered in `supervisors` as a lightweight watched target with its **own capture path** —
  **NOT** a `goal_sessions` row, so `watchdog.Pass` never auto-resumes it. If the master pane goes
  IDLE_AT_PROMPT while open items exist, or WEDGED, that is an *independent* liveness signal even if
  heartbeats are (falsely) still arriving → `master_absent` with `detail=master_pane_idle`. Neither
  source is trusted alone (catches the falsely-heartbeating-but-wedged master).

### 1.7 Per-agent-family verb table (missing_everywhere #1 — resolved)
The control verbs `/goal resume` and the `/goal execute …` launch payload are **Codex builtins**;
the master and many epics run **Claude Code** (different goal/loop mechanisms). Hardcoding `/goal`
would type Codex slash-commands into Claude panes — the exact class of bug migration-numbering
discipline exists to prevent, but for keystrokes. **Design answer:** a table-driven
`internal/verbs` registry keyed on `agent_family` → `{resume, launch(spec_path), nudge_enter,
escape_modal, clear_context}` keystroke templates. Every control-plane send resolves its verb
through this table using the target's recorded `agent` family. Provider-neutral (no literals
outside the allowlisted table). The existing watchdog auto-resume and Phase-2 launch are migrated
onto it (Phase 5). Master free-text replies are DATA and bypass the table (§1.5).

---

## 2. Q2 — Session digests for cheap on-task watching

### 2.1 One call, all sessions (ETag/304 cheap poll)
```
GET /v1/epics/digest   (auth; supports If-None-Match / ETag)
  → { generated_at, digest_seq, master:{registered,last_hb_age_s,open_items},
      epics:[ EpicDigest, … ], attention:[ AttentionItem, … ] }
```
Deterministically compiled from stored state (`epics.status_*`, `goal_sessions` observations,
`account_windows`, `attention_items`) plus the last cached pane capture — **no LLM, no master-side
`tmux capture-pane`**. `digest_seq` is a monotonic counter bumped whenever any epic's observable
state changes; a poll with nothing changed returns **304** — the "everything's fine, sleep" the
`/loop` supervisor needs to burn near-zero context (throughput + reliability steal).

`EpicDigest` (per epic): slug/repo/branch/host/agent; `account{email,model,session_pct,weekly_pct,
severity,resets_at,probe_stale}`; **`context_pct`** (§12.4, disk-derived); `lifecycle`,
`status_state`; `steps{current,total,checked,delta_since_last,checklist[{step,checked,evidence}]}`;
`pane_state` (tmuxio classifier); `ages{pane_change_s,status_update_s,last_commit_s}`; `blockers`;
`base_drift{commits_behind_main, epic_step_commits}`; `drift{signals[],spotcheck}`; `attention[]`;
bounded `pane_tail` (≈20 lines, control-bytes stripped, **explicitly delimited as UNTRUSTED
data** so a hostile pane cannot inject instructions into the master's reasoning — served only on
`GET /v1/epics/{id}/digest?tail=1`); and a deterministic **`on_task`** rollup (throughput steal):
`true` iff pane WORKING/IDLE-with-recent-progress AND no open halting item AND no fired drift signal
AND account not critically capped AND `context_pct` above floor. Lets a master eyeball a 10-epic
fleet in one screen and descend only where `on_task=false`.

### 2.2 How the master consumes it — `flowbee master poll`
`flowbee master poll --json` = **heartbeat + digest(all) + lease top-K attention in one call** (UX
steal — fewest round trips). The skill loops: `poll` → on 304 sleep → for each leased item judge
from digest/tail → `resolve` → every K iterations `/clear` and re-`poll` from scratch. Durable
state means nothing is lost across the reset — the productionized reviewer-watchdog `/clear` +
processed-SHAs discipline, now server-side.

### 2.3 Drift detection — deterministic signals gate a leased spot-check
Signals computed deterministically in the control plane each digest build:

| signal | condition (deterministic) | routing |
|---|---|---|
| `out_of_scope_diff` | epic-branch `git diff --name-only` vs main has a path matching NO scope glob | **→ `scope_violation` (prio 5), no spot-check** |
| `scope_breach_from_main` | a merge to main touched paths INSIDE this epic's reserved globs | **→ `scope_violation`, reverse direction (reliability steal)** |
| `evidence_gap` | `status_state=done` but Phase-3 evidence gate would deny | **→ `epic_finished` w/ deny_reason, straight to master** |
| `claim_exceeds_commits` | checked steps ≫ count of `Epic-Step:` trailer commits on branch (mirror-readable) | **→ `drift_suspect` (strong), no spot-check (throughput steal)** |
| `test_rigging` | epic diff removes assertions / adds `t.Skip`/`if true`/tautological asserts in test files (§12.7) | **→ `drift_suspect` (strong) + flag reviewer brief** |
| `step_regressed` | current_step decreased or a checked box became unchecked | drift_suspect (strong) |
| `status_stale_while_working` | pane WORKING AND status_update_age > T AND no commit in window | soft → advisor |
| `commit_rate_zero` | pane WORKING, zero new commits on branch over window | soft → advisor |
| `wedged` | pane WORKING but last_change_age > T_wedge | soft → advisor |
| `context_low` | `context_pct` < floor (default 15%) — self-degradation risk (§12.4) | soft → advisor (NOT a false stall) |

**Soft** signals are where "off-task or just thinking hard?" is judgment. That judgment is **leased
to a cheap `drift-advisor`** (the `internal/advisor` precedent): a read-only, **anti-affinity**
(different family from the builder) one-shot `claude -p`/`codex exec` seeing digest + diff summary +
signal, returning `{ON_TASK|DRIFTING|BLOCKED, note}`. `ON_TASK` records and dismisses (does not
bother the master); `DRIFTING`/`BLOCKED` enqueues `drift_suspect` with the note. The advisor is an
*independent second actor*, so catching misbehavior does not depend on the possibly-gaming master's
diligence. Spot-checks are **rate-limited per epic** (a persisted budget like the resume budget) so
a genuinely-slow step cannot spawn an advisor every tick. **Mid-epic self-compaction is modeled**
(§12.4): a compaction event resets `context_pct` upward and is NOT treated as a stall/drift.

---

## 3. Q3 — Dashboard (`/epics`) + operator override drawer

Consistent with `internal/web` (Data-interface read-model + `html/template` + SSE + `go:embed`).
New route/nav tab `EPICS` alongside `BOARD`/`FLEET`. New read-models `EpicBoardRow`,
`AttentionRow`, `MasterStatusRow` on `web.Data`; new `internal/api` endpoints `GET /v1/epics`,
`/v1/epics/attention`, `/v1/epics/master`; a new `epics` SSE topic pinged by the supervision tick.

**Layout (top-of-fold answers "what the fuck is going on"):** a **fleet capacity strip** (M accounts
× 5h/7d% × severity × box-local resets × #epics — UX steal), a **NEEDS-YOU attention lane**
(priority-sorted, red `no-master` banner when orphaned), an **epics-in-flight grid** (epic × steps
bar × pane dot [green WORKING / blue IDLE / amber AWAITING / red stalled] × account/usage × Δage
using the existing gray→amber→red `stageClass` thresholds × attention badge), and an
**infra-incident banner** when `ci_infra_incident` is open (§12.5).

**Human override drawer (UX steal — the 3am takeover surface the other designs under-build).**
Click a card → drawer (does not dim the grid): full `## Status`+checklist w/ evidence, bounded live
pane tail, drift signals, account/usage detail, and intervention history (from the ledger). Every
action is reversible and recorded as an **operator-actor** `ledger.Event`:
- **Send instruction** → `POST /v1/epics/{id}/intervene {text}` → same delivery-verified,
  family-verb-table, ledgered path as a master resolve (actor=operator).
- **Pause/Resume watch** → flips `goal_sessions.enabled` (watchdog stops/starts touching it).
- **Force resume** → one-shot resume bypassing the 3/hr budget (operator override).
- **Abandon** → `AbandonEpicRun` (releases scope+host, disables watch, leaves tmux alive).
- **Kill session** → `tmux kill-session` (confirm dialog).
- **Reassign account** (Phase 6) → repoint `epics.account_id` (relaunch decision).
- **Resolve/dismiss attention** → close an item handled out-of-band.

This drawer is the human backstop against BOTH a gaming agent AND a rubber-stamping master (the
only mechanism that catches a master over-claiming — no automated gate does, §12.8).

---

## 4. Q4 — Limits-aware launch & ongoing capacity

### 4.1 Supersede the guessed-budget branch
**Supersede `feat/windowed-token-budget-capacity`** (migration `0023_preemptive_usage_budget`,
guessed budgets, **not applied**). Its premise — "Codex exposes no live %" — is obsoleted by the
scout finding that both providers expose **real server percentages on disk**. Keep the *idea*
(preemptive, not reactive gating), drop the mechanism. `internal/capacity`'s pure core
(`SelectAccount`/`AtCeiling`/`FoldUsage`/`HasFreeSlot`) is kept as-is; we feed it *true* `UsagePct`.
The 0023 number is reclaimed (never applied); nothing on main renumbers.

### 4.2 acctprobe ground truth → `account_windows` (migration 0028)
`internal/acctprobe` (exists on `feat/acctprobe`) yields `LimitWindows{WeeklyPct, SessionPct,
Critical, Staleness, ResetsAt}` per account from `.claude.json cachedUsageUtilization` / Codex
rollout `rate_limits.*.used_percent`, refreshed rarely (direct endpoint 429s). A new
**consolidated supervision ticker** (§12.2) folds these into a new `account_windows` table (keyed on
`account_uuid`, so config-dir≠account is respected; boxes sharing a login fold into one bucket) via
`UpsertAccountLimits`. `worker_accounts.usage_pct` is kept in sync as `max(session,weekly)` so the
legacy ceiling gate and `SelectAccount`/`FoldUsage` keep working unchanged.

### 4.3 Limits-aware launch (`epicAccountGate` replaces `epicQuotaGate`)
1. Filter the epic's `agent`→`model_family` accounts to those with **weekly headroom**
   (`weekly_pct < ceiling` default 90 AND `severity != critical`). Weekly is the epic-killer; a 5h
   dip self-heals via the watchdog's auto-resume, a weekly cap strands the box for days.
2. `capacity.SelectAccount` picks lowest `preference_rank` among those.
3. **Anti-collocation (throughput steal):** prefer an account **not currently powering another
   active epic** (`epics WHERE account_id=? AND state active`). Never double-book one weekly budget
   across two multi-day epics.
4. **Context-% input (§12.4):** if a candidate pane already exists at low `context_pct`, deprioritize
   it — don't hand a big goal to a session at 23% context.
5. If NO account has weekly headroom → **refuse launch** with a clear reason (don't start an epic you
   can't finish). **Exception preserved:** on a *probe error* (can't read acctprobe at all), **fail
   open** with a logged warning — a broken probe must not ground the fleet (same degrade-to-inert
   posture as the watchdog). **Stale data (§12.14):** a `severity=critical` read older than `T_stale`
   is treated as fail-open-with-warning, NOT a hard refusal, so a flaky ssh link cannot phantom-block.
6. **Bind** the choice: `epics.account_id` + `epics.builder_model` recorded; `FLOWBEE_ACCOUNT` set on
   the launched pane env (the pane→account probe key).

### 4.4 Continuous mid-epic monitoring & policy
The supervision ticker compares each active epic's **bound** account against thresholds:
- **5h approaching 100%:** informational (digest badge only) — the watchdog auto-resumes after the
  short reset. **Throughput-preserving default: push through.**
- **7d `severity=critical` (non-stale):** raise `usage_critical` (dedup per account). Master decides
  (leased judgment — a real trade-off): **ride** (near done, finish before reset), **pause** and
  resume after box-local `resets_7d_at`, or **reassign** (expensive relaunch, loses in-context
  memory — master-driven `flowbee epic reassign`, never automatic). **Stale (§12.14):** if the
  critical read is older than `T_stale`, suppress the item and show only a `probe_stale` badge — do
  not fire phantom criticals off stale data over a flaky link.
- We never auto-reassign an account mid-epic (hot-swapping credentials under a running agent is unsafe).

### 4.5 Fleet view + placement planner (throughput steal)
`GET /v1/epics/placement` + `flowbee epic plan`: a pure, replayable, advisory read-model — given free
boxes, headroom-eligible non-collocated accounts, and pending epics, it proposes the max-concurrency
assignment that never double-books a weekly budget. "2 free boxes, 1 account with weekly headroom →
start 1 more epic, not 2." The throughput dial made visible; the master/operator triggers the starts.

---

## 5. Q5 — Single-PR epic risks ("what am I missing")

The one-PR-per-epic contract stays; every mitigation is additive.

**(a) Review dilution.** Three layers, cheapest first: (1) Epic-Step trailers → reviewer brief
instructs commit-by-commit review; (2) **non-blocking checkpoint reviews at step boundaries** — when
a step-completion commit lands (trailer detected during ingestion) and CI is green, optionally lease
a lightweight per-step reviewer (read-only, advisory, **never gates** forward progress); notes
accumulate into `epic_review_notes` and are handed to the final reviewer so the single review event
is not cold-reading 3,000 lines; off-by-default per epic (`checkpoint_review:` frontmatter opt-in to
control cost); (3) granularity (f) keeps epics reviewable in the first place.

**(b) Anti-affinity with one review event (HARD gate — reliability's most load-bearing property).**
`epics.builder_model` recorded at launch from the account/pane resolution (not config intent). When
the epic PR enters review, the lease carries a **required-exclusion**: reviewer family MUST differ
from `builder_model`, reusing the F5 identity-lease machinery. Because there is exactly one review
event this is a **hard lease predicate, never a same-family fallback**: no eligible different-family
reviewer → the PR **WAITS in review** (surfaced as an attention item), never a same-family
self-review slipping through (addendum 11). Checkpoint reviewers inherit the same exclusion.

**(c) Red-CI-at-the-end bisect pain.** Contract amendment (Phase 4): **open a DRAFT PR early** (after
the first step-completion push), ready only at finish; **push per step-completion commit**. CI runs
per push; Flowbee reconciles CI facts on the epic head (reuse existing CI reconciliation) and raises
`ci_red_on_epic_pr` (dedup per head SHA) the moment it goes red — localized to the breaking step, not
hour 40. Auto-resolves on a later green. **Real vs infra-flake classified (§12.5)** so 3am doesn't
page the master to fix a phantom.

**(d) Concurrent scope collisions & main moving.** Launch-time atomic scope-overlap gate (exists)
blocks two active epics sharing paths. **Keep the rebase ban.** Surface `base_drift.commits_behind_
main` in the digest; past a threshold raise low-priority `merge_main_suggested`; the sanctioned remedy
is a **`## Amendments`-authorized `git merge origin/main` (never rebase)** the master authors as an
`amend` resolution — a REAL contract term (INSTRUCTIONS.md line 10), not a fabricated one. Auto-merge
is deliberately NOT automatic (conflict risk mid-step). The reverse breach — a main-merge landing
inside an active epic's reserved tree — fires `scope_violation` (§2.3, reliability steal). **A
dispatch-time cross-epic file-touch detector (§12.6)** catches two epics building the same file even
when globs didn't predict it. The real mitigation is granularity (f).

**(e) A session lying in `## Status`.** Layered: Phase-3 evidence gate (State: done + every step
checked with non-empty evidence + empty blockers + in-scope; **fails closed** on transient mirror
errors) + reviewer brief; the `evidence_gap` drift signal catches claimed-done-but-unevidenced
*before* the PR opens; the `claim_exceeds_commits` deterministic check (throughput steal) catches
status-vs-reality *before any reviewer*; **`verify_evidence` DEFAULT-ON** (operator-UX steal, flipping
reliability's opt-in): at the merge gate Flowbee **re-executes the epic's `Validate:` commands** in a
fresh checkout at PR head and compares to claimed evidence + reconciled CI, **never trusting the
agent-authored evidence string** — resolved as a **leased worker job with a defined host/cost**
(operator-UX left this hand-waved; we don't). A lying `## Status` cannot manufacture a green CI fact
(verdicts derive from reconciled facts). **Residual (§12.7):** re-running a *rigged* test still passes;
the `test_rigging` deterministic tripwire + the cross-family reviewer reading the diff are the
backstop — no automated gate fully closes vacuous-Validate; consciously flagged to the reviewer.

**(f) Granularity guidance.** Lives in the `author-epic` skill AND `epics/INSTRUCTIONS.md` AND this
doc (Q10, §10). An epic is ONE branch, ONE coherent reviewable story, sized to land in **hours not
days**, **≤ ~10–12 steps**, scope non-overlapping with active epics, burn within the chosen account's
weekly headroom. **Split** when steps > ~12, scope spans unrelated subsystems, two independent
deliverables, or wall-clock > weekly runway; **sequence** dependent epics (B `blocked_by` A's merge)
rather than one mega-epic; **parallelize** only disjoint-scope epics. `flowbee epic start --plan`
dry-runs step count + scope-overlap + selected-account headroom before committing.

---

## 6. Q6 — Plane B reconciliation
**One coherent answer: retire the external bash pool over time; small work stays in flowbee's
existing per-issue lane; the epic lane does NOT grow a second native label-pool.** Running the
external `codex_watch_needs_codex.sh` alongside flowbee is two systems of record for the same small
work; a *native* label-pool would duplicate the job engine and reintroduce the CI/merge contention
the epic lane exists to escape. Plane B's *hardening* is absorbed as **epic-lane supervision
behaviors**, not a pool: atomic-mkdir-claims → fenced epoch leases (already stronger);
cooldown/giveup → lease TTL + requeue budget (present); **3h runtime-kill of wedged trees → a
`wedged`/`stalled` escalation ladder ending in operator-authorized session kill (new, Phase 9)**;
**MAX_OPEN_PRS backpressure → per-repo open-PR backpressure gate at dispatch (new, Phase 9)**;
orphan-worktree sweep + stale-lock reclaim → janitor + lease liveness (present); blocked-by promotion
→ scheduler (present). Net new: two small dispatch/worker guards. **Big work → epics, small work →
per-issue lane, ONE system.** The russ script keeps running until the two lanes demonstrably cover
its cases, then retires.

---

## 7. Q7 — Phase 3 branch disposition
**LAND `worktree-agent-a6dfd87471ca7e53b` (f905a91 + 7372c45), rebased, as Phase 4's opening act.**
It is precisely the anti-lying integrity the reliability priority and Q5(e) demand (evidence gate +
reviewer brief, **failing closed** on transient mirror errors); it reuses the content-integrity gate;
it carries F1–F6 including the **F6 AddEpicHost argv-validation fix still OPEN on main** (a real
ssh-option-injection hole); and it has **no migration** → near-zero conflict. Reconciliation before
landing: (1) **rebase onto current main** and drop the stale `-90` deletion of `epics/INSTRUCTIONS.md`
(it predates cff0d3a — must not clobber the standing contract); (2) carry F6 forward regardless; (3)
fold `epicspec/{detect,evidence,glob,brief}.go` + `store/epicgate.go` as-is and wire `EpicForHeadSHA`
detection into review-job creation with the §5b anti-affinity exclusion. Later, additively: the
evidence-deny and CI-red paths also enqueue attention items once the queue exists (Phase 8); the
`verify_evidence` re-execution extends `epicgate.go` (Phase 8). No rework, no supersede — it is on the
critical path for safe epic merges.

---

## 8. Q8 — The invocation skills
**Location: repo `.claude/skills/` (versioned, shareable, shipped with the control plane it drives);
a thin `~/.claude/skills/*` shim `@`-includes the repo copy for muscle memory.** Replaces the current
`~/.claude/skills/author-epic`.
1. **`author-epic`** (upgraded): interview intent → write `epics/YYYY-MM-DD-<slug>.md` with ordered
   `## Steps` (each with `Validate:`), `scope:` globs, optional frontmatter (`agent:`,
   `checkpoint_review:`, `verify_evidence:`), applying the granularity rubric (§10) AND **querying
   flowbee for currently-reserved scopes** (`flowbee epic plan`) so the planner designs AROUND live
   reservations (Q10b) → validate via `--plan` dry-run → commit to main (spec-immutable once launched)
   → `flowbee epic start`.
2. **`supervise-epics`** (new, master-facing): `flowbee master register --label master-a` → loop
   `flowbee master poll --json` (heartbeat+digest+lease) → judge → `flowbee master resolve <id>
   (--reply|--ack|--escalate)` → **every K iterations `/clear` and re-poll** (the baked-in
   context-reset discipline; the skill explains *why* it is safe — durable state is the source of
   truth). The productionized reviewer-watchdog cadence-reset.
3. **`flowbee-conductor`** (umbrella): author N epics → launch onto free boxes/accounts → register as
   master → supervise to labeled PRs. One command for "drive this whole slate forward."

CLI the skills wrap (all new below): `flowbee master register|poll|resolve|status`,
`flowbee attention list|resolve`, `flowbee epic digest|plan|reassign|intervene|pause|resume|kill`.

---

## 9. Q9 — Phasing (concrete, landable-in-order)

Migrations continue after **0026** (main tops at `0026_epics.sql`; double-0023/0024 already applied,
keyed by full filename so runtime is unaffected — but see the **migration-number allocator (§12.6,
Phase 4)** that prevents future collisions). `serve`/`api` are shared touch-points; phases that both
edit them are merge-ordered. **Critical path: P4 → P5 → P6 → P7.** P8 and P9 branch off after P5/P6.
Foundation gates: `feat/tmuxio-primitives` → P5; `feat/acctprobe` → P6.

### Phase 4 — Land review gate + contract amendments + migration allocator  ⟨CP start⟩
- **Goal:** epic PRs review against their own criteria; evidence gate live; draft-PR-early +
  merge-main-not-rebase codified; the migration-ladder collision closed before parallel builders start.
- **Files:** land Phase-3 branch rebased — `internal/epicspec/{detect,evidence,glob,brief}.go`,
  `internal/store/epicgate.go`, `internal/project/project.go` (`epicDenyReason`),
  `internal/worker/review.go`, `client/client.go`, `internal/api/server.go`; `epics/INSTRUCTIONS.md`
  amendments (draft-PR-early, push-per-step, merge-main-via-`## Amendments`); F6 in
  `internal/store/epichost.go`. NEW: `internal/store/migrations/LADDER.md` (reserved-number ladder) +
  `flowbee migration reserve <slug>` allocator + a CI check that fails on a duplicate unreserved
  number (§12.6).
- **Migrations:** none (Phase 3 has none; LADDER is a doc+tool, not a migration).
- **Tests:** land Phase-3's `epicgate_test`, `merge_epic_gate_test`, `review_test`,
  `epic_criteria_test`; new acceptance: epic PR with a claimed-but-undiffed step → `merge_handoff`;
  allocator rejects a duplicate number.
- **Demoable:** an epic PR with a lie in `## Status` is denied autonomous merge; two builders cannot
  grab the same migration number.
- **Parallelizable:** single-owner (touches review path + contract); land first, alone.

### Phase 5 — Attention queue + master registry + verb table + launching-reaper  ⟨CP⟩
- **Goal:** typed, fenced, deduped attention items; master register/heartbeat/lease/resolve with
  exactly-once-in-practice verified delivery + send-and-ack; per-agent-family verb table; reap
  stranded `launching` rows; consolidated supervision ticker skeleton.
- **Depends on:** `feat/tmuxio-primitives` (delivery-verified send, pane classifier).
- **Files:** `internal/store/{attention,supervisor}.go`; new pure core `internal/attention`
  (priority/dedup/fence decisions over injected values); new `internal/verbs` (family verb table);
  `internal/api/server.go` (+master/attention endpoints); `internal/watchdog` producers
  (`needs_input`/`blocked`/`wedged` → `UpsertAttentionItem`; auto-resume migrated onto `internal/verbs`);
  `cmd/flowbee/{master,attention}.go`; `cmd/flowbee/serve.go` (**one consolidated `epic-supervision`
  ticker** — reaper + producers + master-liveness, §12.2). New `ledger.EventKind`s
  (`attention_opened/leased/resolved/escalated`, `master_registered/absent`, `epic_intervention`).
- **Migrations:** `0027_epic_attention.sql` — `supervisors` (label UNIQUE, epoch, state,
  last_heartbeat_at, box, tmux_name, model_family) + `attention_items` (kind, epic_id, dedup_key with
  partial-UNIQUE on open|leased, priority, state, leased_by, item_epoch, lease_expires_at,
  delivery_key, evidence, detail, resolution, verdict, occurrences, first/last_seen_at, resolved_at).
- **Surface:** `POST /v1/masters/register|/{id}/heartbeat`, `POST /v1/masters/attention/lease`,
  `POST /v1/masters/attention/{id}/resolve`, `GET /v1/masters/attention`; `flowbee master
  register|poll|resolve|status`, `flowbee attention list`.
- **Tests:** unit (fence rejects stale epoch; dedup upsert; expiry sweep; **crash-window pane
  re-check idempotency**; launching-reaper reaps stale row + releases host/scope; verb table routes
  Codex vs Claude resume correctly); acceptance (register → blocked session raises item → master
  leases → replies → tmuxio delivers via fake Runner → verify strong → awaiting_ack → next digest
  acks → ledger entry; no-master → `master_absent` alarm; master death reclaims leases).
- **Demoable:** a master supervises a blocked epic end-to-end via HTTP; ledger shows the intervention;
  a SIGKILL'd launch no longer strands a host.
- **Parallelizable workstreams:** {store+api}, {watchdog producers + verb table}, {reaper + ticker}.

### Phase 6 — Digests + acctprobe capacity + account/context binding  ⟨CP; parallel tables w/ P5, merge-order serve/api⟩
- **Goal:** real 5h/7d % + context% per session; limits-aware launch; per-epic digest; `flowbee master
  poll` complete; auth-dead + stale-usage handling.
- **Depends on:** `feat/acctprobe`. Supersedes `feat/windowed-token-budget-capacity`.
- **Files:** `internal/store/capacity.go` (`UpsertAccountLimits`; `usage_pct=max(session,weekly)`);
  new pure core `internal/epicdigest` (digest assembly + `on_task` rollup over injected state); new
  `internal/ctxprobe` OR acctprobe extension (context% from Codex rollout `total_token_usage/
  model_context_window` + Claude transcript per-message usage, §12.4); `cmd/flowbee/serve.go`
  (acctprobe fold folded INTO the consolidated ticker; `usage_critical` producer with staleness gate;
  auth-dead classifier); `cmd/flowbee/epic.go` (`epicAccountGate` + `--plan` + reassign);
  `internal/api` (`GET /v1/epics/digest` ETag, `/{id}/digest?tail=1`, `/v1/epics/placement`).
- **Migrations:** `0028_epic_capacity.sql` — `account_windows` (account_uuid PK, email, model_family,
  five_hour_pct, seven_day_pct, session_pct, weekly_scoped_pct, severity, resets_5h/7d_at,
  fetched_at_ms, probe_stale, reported_at) + `epics.{account_id, builder_model, base_sha,
  context_pct, pane_state, pane_tail, last_commit_at, auth_state}`. 0023-guess columns never applied.
- **Tests:** unit (fold real %; launch refusal on no-weekly-headroom; fail-open on probe error;
  anti-collocation; box-local resets; **stale-critical suppression**; context% parse from a rollout
  fixture; auth-dead classifier detects pane-at-shell + revoked token); acceptance (probe fixture →
  digest true %; low-context session deprioritized at launch; `flowbee master poll` returns fleet +
  leased items).
- **Demoable:** `flowbee master poll` shows each epic's account 5h/7d% + context%; a start is refused
  when no account has weekly headroom; a pane dropped to a shell surfaces `auth_dead`, not a resume loop.
- **Parallelizable:** {capacity+acctprobe fold}, {digest+ctxprobe}, {launch gate}.

### Phase 7 — Operator dashboard `/epics` + override drawer  ⟨depends P6 digest read-model⟩
- **Goal:** the operator's eyes + the 3am human-takeover surface.
- **Files:** `internal/web/{web.go,handlers.go}` (`Data += EpicBoard/AttentionQueue/MasterStatus`),
  `internal/web/templates/epics.html` + drawer, nav tab, new `epics` SSE topic; `internal/api`
  (`GET /v1/epics`, `/v1/epics/attention`, `/v1/epics/master`; override `POST /v1/epics/{id}/intervene`,
  `.../control {pause|resume|force_resume|abandon|kill}`).
- **Migrations:** none.
- **Tests:** `web_test` off a fake Data (cards render epics×steps×pane-dot×usage×attention badge;
  infra-incident banner shows); api handler tests (override posts route + ledger operator-actor event).
- **Demoable:** operator sees the fleet live via SSE; sends an instruction, pauses a watch, force-resumes,
  and kills a session from the drawer — each ledgered.
- **Parallelizable:** {web+templates}, {override api} once P6 read-models exist.

### Phase 8 — Drift + CI cadence + checkpoint reviews + anti-affinity handoff + anti-lying teeth  ⟨depends P5 items, P6 digest⟩
- **Goal:** deterministic drift signals; mid-epic CI with flake classification; non-blocking step
  reviews; completion-triggered cross-family review; default-on evidence re-execution; test-rigging +
  cross-epic collision detectors; master-resolution audit.
- **Files:** `internal/store/epicdrift.go` (signals §2.3 incl. `claim_exceeds_commits`, `test_rigging`,
  mirror commit-count); consolidated-ticker drift/CI pass; `internal/worker` (checkpoint reviewer lease
  + spotter advisor via `internal/advisor` + **completion-triggered anti-affinity review handoff**,
  addendum 11); `internal/worker/review.go` (family-exclusion predicate); `internal/store/epicgate.go`
  (`verify_evidence` DEFAULT-ON re-execution as a leased worker job); `internal/project` (CI-flake
  classifier §12.5; main-merge-into-scope detection); `epic_finished`-dismissal audit (§12.8);
  cross-epic file-touch collision detector (§12.6).
- **Migrations:** `0029_epic_drift_ci.sql` — `epic_review_notes` + epic CI-fact columns + per-epic
  spot-check/verify budgets.
- **Tests:** unit (each signal fires exactly on its condition; `claim_exceeds_commits` vs mirror
  fixture; `test_rigging` catches removed-assertion/added-skip; anti-affinity lease rejects same-family
  reviewer, WAITS when none; evidence re-run mismatch → `merge_handoff`; flake vs real CI classified;
  false `epic_finished` dismissal re-opens on reconciled no-PR); acceptance (out-of-scope diff →
  `scope_violation`; red CI → `ci_red` at the breaking step; finished Codex epic → Claude reviewer →
  gate re-runs evidence → merge or handoff).
- **Demoable:** a lying/rigged status or out-of-scope diff is caught before review; an epic is reviewed
  only by a different model family; a phantom CI flake is auto-rerun, not paged.
- **Parallelizable workstreams:** {drift signals}, {CI cadence+classifier}, {review handoff+evidence
  re-run}, {checkpoint reviews}.

### Phase 9 — Missions, post-merge verify, Plane-B guards, planner, skills, artifact registry  ⟨parallel after P5/P6⟩
- **Goal:** the level ABOVE a single epic (chained epics + named gates + invariants), post-merge
  live-verify, small-work backpressure, one-command supervision.
- **Files:** `internal/store/mission.go` + `0030_missions.sql` — `missions` (chained epics via
  `blocked_by`), `mission_gates` (named refuse-to-skip gate conditions, e.g. `audit-clean-before-flip`),
  `mission_invariants` (per-mission INVARIANT declarations injected into review briefs, §12.9),
  `post_merge_stages` (per-repo configurable verify hooks — generic, no railway/deploy specifics in
  core, §12.9), `artifacts` (registry of session-generated files so nothing strands, §12.10);
  per-repo open-PR backpressure gate + worker runtime-kill of wedged trees (`internal/worker`,
  Plane-B absorption); `flowbee epic plan` placement planner (§4.5); `.claude/skills/{author-epic,
  supervise-epics,flowbee-conductor}` + `docs/design/epic-lane.md`.
- **Migrations:** `0030_missions.sql`.
- **Tests:** unit (mission gate refuses to skip; invariant reaches review brief; post-merge stage
  gates completion; backpressure holds at MAX_OPEN_PRS but allows PR-fix; runtime-kill reaps a stuck
  tree; artifact registered on PR open); acceptance (chained epics respect blocked_by + named gate;
  post-merge verify hook fails → mission not marked done).
- **Demoable:** a mission of 3 chained epics with an `audit-clean-before-flip` gate the system refuses
  to skip; small + big work coexist in one flowbee; `flowbee-conductor` drives the fleet with one command.
- **Parallelizable workstreams:** {missions+gates+invariants}, {post-merge verify}, {Plane-B guards},
  {skills+docs}, {artifact registry}.

---

## 10. Q10 — Goal-sizing doctrine (committed)
Lives in THREE places (so every future master gets it): this doc, `epics/INSTRUCTIONS.md`, and the
`author-epic` skill. **Rubric:** one epic = ONE branch, ONE coherent reviewable story, ONE subsystem,
sized to complete in **hours not days**, **≤ ~10–12 steps**, scope disjoint from every active epic's
reserved globs, expected burn within the chosen account's **weekly headroom**. **Split into sequential
epics** (B `blocked_by` A's merge) when there is an ordering dependency; **split into parallel epics**
only when scopes are provably disjoint; **keep as one** when steps share files or have strict ordering
(splitting tightly-coupled work creates merge-ordering pain worse than a big PR). **(b) Authoring-time
feedback loop:** the skill calls `flowbee epic plan` to read currently-reserved scopes and designs
AROUND live reservations; the atomic launch-time `ScopeOverlap` gate stays as the backstop. **(c)
Advisory vs hard gate — DECIDED: advisory warn, not hard refuse.** `flowbee epic start --plan` warns
when declared step count > 12 or estimated wall-clock exceeds weekly runway, but does not refuse (the
master owns the sizing call; over-refusing grounds legitimate work). The ONE hard refusal stays
scope-overlap (a correctness invariant) and no-weekly-headroom (can't-finish). **(d)** Committed to the
doc + skill + contract.

## 11. Q11 — Completion-triggered cross-family review handoff (committed)
When status ingestion sees `State: done` and the one PR opens (or Phase-0 auto-adopts the
`needs-claude` PR), the supervision ticker fires `epic_finished` and Flowbee **auto-triggers the
review handoff**: `epics.builder_model` (bound at launch from the account/pane resolution, NOT config
intent) drives a review lease carrying a **required family-exclusion**. Staffing: the **existing
`code_review` worker lease** with the anti-affinity predicate injected (reuse F5), composed with Phase
3's criteria-driven reviewer brief (the brief supplies the epic's criteria+checklist; the exclusion
supplies the reviewer identity). Codex-built → Claude-reviewed; Claude-built → different-family-
reviewed. **No opposite-family capacity → the PR WAITS in review, surfaced as an attention item —
never a same-family silent fallback** (hard predicate, §5b).

---

## 12. Q12 — Orchestrator ops spec fold-in + every `missing_everywhere` item (resolved)

Each judge-flagged gap gets a concrete mechanism or a consciously-accepted risk with rationale.

**12.1 Structured session-state, disk-preferred (ops #1).** The digest fuses **disk-state (preferred)
+ pane-state (only what disk can't see)**: goal status + context% + usage come from disk (Codex
rollout JSONL, `goals_1.sqlite`, Claude transcripts, `.claude.json`); pane classification covers only
wedged UI / dialogs / queued-unsent input. `{type,host,model,goal,state,context_pct,last_activity,
current_action,branch,worktree,queued_message}` is the `EpicDigest` (§2.1). *Phase 6.*

**12.2 SQLite write-contention budget (missing_everywhere).** ~12 tickers already run against the
single-writer DB (MaxOpenConns=1, busy_timeout=5s); adding 3–5 write-heavy tickers risks a blocked
write silently dropping a pass under a saturated box. **Resolved: one consolidated `epic-supervision`
ticker** does the whole epic pass (digest compile + drift + attention producers + launching-reaper +
master-liveness) in a single serialized batch, and the acctprobe/capacity fold runs on a **staggered**
5-min offset. Aggregate new write budget: 2 tickers, not 6. *Phase 5 (skeleton) + Phase 6 (fold).*

**12.3 Send-and-ack of PROCESSING (ops P0).** Delivery verification proves *submission*; a
`state=awaiting_ack` stage (§1.5 step 4) proves *processing* — the next digest confirms
`pane_state`/`## Status`/commit advanced within `T_ack`, else re-opens `steer_not_processed`. A
politely-stalling agent that absorbs a nudge and keeps drifting is caught. *Phase 5 delivery + Phase 6
digest.*

**12.4 Per-session context-% + auth-dead + self-compaction (ops #1 ask, ADDENDUM 2).** Context% is
disk-derivable (`internal/ctxprobe`: Codex rollout `total_token_usage/model_context_window`; Claude
transcript per-message usage) → a digest field, a launch-selection input (don't hand a big goal to a
23%-context session), and a soft `context_low` drift signal. **Mid-epic self-compaction is modeled:** a
compaction event (context% jumps up) is recognized and NOT fired as a stall/drift. **Auth-dead is a
NEW distinct state** (`epics.auth_state`, `auth_dead` attention kind, prio 10): detected by (a) disk —
revoked/absent refresh token — and (b) pane — classifier sees the pane dropped to a bare `$` shell
(distinct from IDLE_AT_PROMPT). Routes to a **human-only guided re-auth flow**, NEVER the auto-resume
loop (which would burn the hour). Categorically separate from limit-blocked (wait for reset) and
wedged (in-terminal recovery). *Phase 6.*

**12.5 CI real-red vs infra-flake (ops most-emphasized P0).** A deterministic classifier over CI
annotations/logs flags golangci-timeout / GitHub-503-repo-wide / box-saturation signatures as
`suspected_flake`: `ci_red_on_epic_pr` carries the flag; a suspected flake is **auto-rerun once**
before paging; repeated flakes across the fleet open a fleet-level `ci_infra_incident` rendering an
**infra-incident banner** on the dashboard. A real red still pages the master. *Phase 8.*

**12.6 Migration-number ladder + dispatch-time collision detector (P0, self-inflicted).** The runtime
keys migrations on full filename (double-0023/0024 work), but concurrent branches silently pick the
same NUMBER. **Resolved: single-owner-of-migrations discipline + a reserved-ladder file
(`internal/store/migrations/LADDER.md`) + `flowbee migration reserve <slug>` allocator** that
atomically appends the next number, + a **CI check that fails a PR introducing an unreserved or
duplicate number**. Separately, a **dispatch-time cross-epic collision detector** flags two epic
branches whose diffs touch the same file even when scope globs didn't predict it (path-based globs
can't see the *number* collision or an unforeseen shared adapter). *Phase 4 (allocator) + Phase 8
(cross-epic detector).*

**12.7 Test-rigging / vacuous-Validate (adversarial-review checklist §3.2).** A deterministic
`test_rigging` tripwire diffs the epic branch's test files for removed assertions, added
`t.Skip`/`if true`/tautological asserts → strong `drift_suspect` + a flagged region injected into the
reviewer brief. **Consciously-accepted residual:** re-running a rigged test still passes, and only the
cross-family reviewer reading the diff catches a genuinely vacuous Validate — no automated gate fully
closes it. We do not pretend re-execution is sufficient; the tripwire + the hard cross-family reviewer
+ the brief flag are the layered defense. *Phase 8.*

**12.8 Audit the MASTER's own resolutions (missing_everywhere).** The master is an LLM that "over-
claimed once." **Resolved:** (a) deterministically-checkable resolutions are **verified against
reconciled facts** — a `dismiss` of `epic_finished` is checked against the actual PR-opened fact; a
false dismissal (no PR) re-opens the item; (b) the advisor independent-check is **extended beyond
drift** to `epic_finished` and `usage_critical` (a cheap second actor pre-filters, not only the
master); (c) the operator dashboard's over-claim/orphan banner is the human backstop that catches a
rubber-stamping master (no automated gate does). *Phase 8.*

**12.9 Mission gates above epics + post-merge live-verify + per-mission INVARIANTs (ADDENDUM 2).**
`missions` (chained epics via `blocked_by`) + `mission_gates` (NAMED refuse-to-skip conditions, e.g.
`audit-clean-before-flip`, tracked state the system refuses to skip) + `mission_invariants` (per-
mission INVARIANT declarations — never-bury, review-before-prod, merge-only-reviewed-head — injected
into review briefs). **Post-merge is part of the pipeline:** `post_merge_stages` are **per-repo
configurable verify hooks/gates (generic, no deploy-provider specifics in core)** — merged → deployed
→ LIVE-VERIFIED; a mission is not `done` until its post-merge stage passes (closes the "deployed ≠
works" over-claim structurally). *Phase 9.*

**12.10 Artifact registry (ADDENDUM 2).** An `artifacts` table registers files sessions generate
(reports, diffs, the epic PR + its worktree path) so nothing strands in an unlocatable worktree
(nearly-lost 3,126-msg audit artifact). Minimal: register on PR open + on checkpoint-review note. *Phase 9.*

**12.11 Watchers/triggers (ADDENDUM 2).** The attention queue's producers ARE the armed conditions
("notify when CI settles / session idles / endpoint recovers / new PR on branch") — replacing hand-
rolled bash polling. `ci_red_on_epic_pr`, `epic_finished`, `usage_critical`, `stalled` are the
first-class watchers; post-merge verify hooks (§12.9) extend to endpoint-recovery. *Phases 5/8/9.*

**12.12 Durable orchestration state reloadable in one call (ADDENDUM 2).** `flowbee master poll` +
`GET /v1/epics/digest` reload the full board (sessions, PRs+stages, attention, missions) in one call;
the master holds no state between `/clear`s. *Phases 5/6/9.*

**12.13 Session-health taxonomy split (ADDENDUM 2).** Three distinct states with three distinct fixes:
`auth_dead` (human re-login, §12.4), `usage_critical`/limit-blocked (wait for reset, §4.4), `wedged`/
`stalled` (in-terminal recovery, §2.3). No longer folded into one `blocked` bucket. *Phases 5/6/8.*

**12.14 Stale-usage coupling over flaky ssh (missing_everywhere).** acctprobe-over-ssh and pane-
capture-over-ssh fail together, so usage goes stale exactly when a box is troubled. **Resolved:**
`account_windows.probe_stale` + explicit policy — `usage_critical` is **suppressed** when the critical
read is older than `T_stale` (no phantom criticals off stale data); the launch gate treats a stale
critical as **fail-open-with-warning**, not a hard refusal (degrade-to-inert, matching the watchdog);
the dashboard shows a `probe_stale` badge so the operator knows the number is old. *Phase 6.*

---

## 13. What you were missing (addressed to the operator)

1. **Your automation can strand a box forever, silently.** `AddEpicRun` writes `state='launching'`
   (which holds the host+scope reservation) BEFORE the tmux session is confirmed up; a SIGKILL of the
   CLI, a hung ssh preflight, or a box reboot mid-launch strands that row and permanently blocks the
   host's one-epic gate — possibly with a live-but-untracked tmux session. None of the three input
   designs reaped it. **Phase 5 adds a launching-reaper.** This is the single most likely way you lose
   a box at 3am, and it was invisible.

2. **The control verbs are Codex-shaped, but your master and half your epics are Claude Code.**
   `/goal resume` and `/goal execute` are Codex builtins; the plan as drafted would type Codex slash-
   commands into Claude panes — the keystroke version of the exact migration-number bug that already
   bit you twice. **Phase 5's per-agent-family verb table** is not optional polish; it is a correctness
   fix, and no input design had it.

3. **"Delivered" is not "done," and nobody was closing that loop.** Every design verified *submission*
   (the input box cleared) and then marked the item resolved — but an agent can absorb a nudge and keep
   drifting. **The `awaiting_ack` stage (Phase 5/6)** re-checks that behavior actually changed;
   otherwise a politely-stalling agent looks handled while it quietly goes off the rails.

4. **You will over-trust your own master.** It is an LLM; it over-claimed once already. Only *drift*
   got an independent second opinion in the input designs — a master dismissing `epic_finished` with no
   PR, or riding a `usage_critical` it shouldn't, was caught by nothing but your eyeball. **Phase 8
   verifies deterministically-checkable master resolutions against reconciled facts and extends the
   advisor pre-filter beyond drift**, and the dashboard drawer is the human backstop.

5. **A green re-run can still be a rigged green.** Flipping evidence re-execution to default-on (which
   we did) catches a *lying* status but not a *rigged* test — an agent that weakens an assertion or adds
   a `t.Skip` gets a rigged-green on the first run AND the re-run. **Phase 8's `test_rigging` tripwire +
   the HARD cross-family reviewer** are the only real defense, and we flag it to you honestly rather than
   claiming re-execution closes it. The safe path (short epics, disjoint scope, one reviewable story) is
   also the fast path — that is the whole point of the granularity doctrine.

Runners-up folded in without ceremony: CI infra-flake classification (so 3am doesn't page you for a
golangci timeout), auth-dead as its own state (human re-login, not a resume loop that burns the hour),
per-session context% (don't hand a big goal to a 23%-context session, and don't fire drift on a normal
mid-epic compaction), stale-usage suppression over flaky ssh, mission gates + post-merge live-verify
(so "merged" stops masquerading as "works"), and a migration-number allocator so your parallel builders
stop re-colliding on 0028 the way they already did on 0023 and 0024.

---

## 14. Cross-cutting invariants (kept, not re-derived)
- **Control plane deterministic, no LLM.** New decision logic (`internal/attention`, `internal/verbs`,
  `internal/epicdigest`, drift signals) is pure-core over injected values; judgment (drift verdicts,
  master interventions, spot-checks) is leased to edges via the advisor/master precedent.
- **Fenced exactly-once leases** for attention items (epoch); verdicts from reconciled facts, never
  worker say-so; Phase-3 content-integrity gate before merge — reused, not reinvented.
- **Closed keystroke verb set preserved for control-plane sends** (now table-driven per family);
  master-authored guidance is a distinct, authenticated first-party DATA channel delivered through the
  same verified-send primitive, never built from pane/scrollback/epic-file content. Pane data reaches
  the master only as delimited untrusted evidence. `shQuote` everything; `ssh -- <host>`; argv
  validation at registration.
- **SQLite single-writer, serialized txs;** new writes go through the store; new work runs on TWO
  consolidated tickers (§12.2), not six.
- **Migrations 0027 → 0030**, never renumbering applied ones; the capacity branch's third 0023 is
  consciously superseded; the LADDER allocator prevents the next collision.
- **`go test ./...` incl. `test/acceptance` green every phase;** provider-neutrality lint honored (the
  `agent`→`model_family` and verb tables stay table-driven, no provider literals outside allowlisted positions).

---

## 15. Field-review amendments (v2 — from the live orchestrator's markup, 2026-07-16)

The running orchestrator session (`russ-claude`) reviewed this design against its live multi-session run
(`~/Desktop/flowbee-spec-review-2026-07-16.md`). Verdict: endorse and build. These amendments are BINDING
refinements; each is wired to a phase.

**15.1 `goal_paused` is a first-class session state** (hit twice in the field: Codex at `Goal paused
(/goal resume)` after moving a PR to ready, and after an auth-death recovery). Healthy process, will NOT
self-proceed; not `idle`, not `achieved`, not `blocked_usage`. Recovery is the family `Resume()` verb with
its own trigger + budget (distinct from the usage-limit path). Watchdog parse/classify + digest carry it;
folding it into `idle` breaks recovery. *Phases 5 (producer/verb trigger) + 6 (digest).* 

**15.2 `stalled` must suppress on long-running subprocesses.** A healthy session sat 46 min on
`make local-ci` with pane-hash static, zero commits, stale step timestamps — all three stall signals dark.
The `stalled`/`wedged` detectors require ALSO that the pane shows no known long-op indicator ("Waiting for
background terminal", an in-flight test/build spinner); when a long-op indicator is present, extend the
window instead of firing. *Phases 6 (classifier patterns) + 8 (drift signals).* 

**15.3 `compacting` + `launching` transient states, detected to SUPPRESS wrong actions.** A compacting
session looks wedged (false escalation) and a steer sent mid-compaction is swallowed (the queued-not-sent
bug via a new door): detect from the pane indicator ("Compacting…") and/or a transcript summary event;
while `compacting`: no steers, no escalation, re-check next tick. `launching` (post-launch, pre-first-
prompt) likewise never classified `unknown`-actionable. **`context_pct` must be post-compaction-accurate**
(verify rollout-tail reset semantics after a summary event — an assignment input computed from
pre-compaction usage is worse than none). *Phase 6.*

**15.4 Escalation tiers corrected (master-first vs human-first).** Human-immediate (no master window):
`auth_dead`, `wedged_ui`, `master_absent`, `launch_failed`. Master-first (~10–15m lease window, human only
if unleased): `usage_critical` (it is a master decision by design — human-immediate would cry wolf),
`drift_suspect`, `ci_red_on_epic_pr`, `needs_input` (tiered: blocking ~10m, non-blocking 15–30m),
`stalled`. `send_unverified`/delivery-failed: fast master retry (~2–5m), not a long TTL. *Phase 5
(`internal/attention` escalation policy).* 

**15.5 Cross-session SEMANTIC collision detection.** The field's worst collision — two sessions
independently building the same adapter — had **disjoint scope globs** and passed every per-session drift
signal; the overlap was semantic. (a) Authoring-time: `flowbee epic plan` surfaces **subsystem adjacency**
(coarse: same top-level subsystem dirs touched by an active epic's branch diff, not just glob overlap) so
the master sees "another active epic is working email-triage" before authoring into it. (b) New
deterministic signal `pr_merged_but_branch_active`: a session still working a branch whose PR merged (the
stale-nudge incident). *Phases 8 (signal) + 9 (plan/authoring).* 

**15.6 Staged post-merge hooks + distinct pipeline states.** A flat post-merge command conflates three
failure semantics the field got burned on: `pre_gate` (blocks the deploy: migration-safety, never-bury),
`action` (the deploy itself; can partial-fail), `post_verify` (asserts the change took effect on the real
surface; fails independently of a successful deploy — "deployed ≠ works"). Each stage records its own
pass/fail; the mission map renders `merged` / `deployed` / `verified-live` as distinct states; a
`post_verify` failure raises an attention item and never marks the chain green. The board also carries the
**prod-vs-main delta** (deployed SHA, undeployed merges, pending migrations) as first-class state. *Phase 9
(`post_merge_stages` schema becomes staged).* 

**15.7 Compaction-recovery completeness on the board.** Beyond entity state, a fresh master needs the WHY
and the thread: `missions.charter_ref` (pointer to the durable decision-log authored by the master — flowbee
links it, never authors it); `supervisors.last_reported_status` (last human-facing update, so a fresh master
continues the thread without re-reporting or contradicting); review-verdict **artifacts** (the verify
outputs, not just the enum) attached as artifact refs; and master-registered **WIP markers** on a PR/epic
(`fix_in_flight {label, started_at, eta}`) so a post-compaction master does not re-dispatch a fix already
running — the one place the no-subagent-tracking rebuttal bites, closed cheaply. *Phases 5 (column) /
6 (board) / 8 (artifact refs) / 9 (charter).* 

**15.8 The cross-family reviewer RUNS the code — non-negotiable.** The field's reviews caught a rigged-test
risk and a real would-have-buried-critical-mail bug only because the reviewer executed targeted tests, not
because it read the diff. The Phase-3 reviewer brief is amended: the reviewer must independently build and
run targeted tests (the epic's `Validate:` set at minimum) — composing with, not replaced by, the
`verify_evidence` re-execution gate. *Phases 4 (brief text) + 8 (handoff).* 

**15.10 Push-to-wake master ping (operator confirmation, 2026-07-16).** The poll loop is the workhorse,
but an idle master pane does not wake itself. When an item at master-first-or-higher priority is enqueued
and the registered master's pane classifies IDLE_AT_PROMPT, flowbee delivers a **fixed-template ping** into
the master's pane via the verified send: `flowbee: <N> attention items pending (top: <kind>). Run: flowbee
master poll` — where `<N>` is a count and `<kind>` comes from the closed kind enum, NEVER free text derived
from pane/scrollback content. Template lives in the family verb table (`NotifyMaster(count, topKind)`);
rate-limited (one ping per T_ping unless the top kind escalates); skipped when the pane is WORKING (the
item waits in the queue the master will poll anyway); never sent to a `compacting` pane (§15.3). This
turns attention latency from "master's poll cadence" into "immediate" without adding any trust surface.
*Phase 5 (verb template) + integration pass (ticker wiring).* 

**15.11 Context management: per-item judgment, self-contradiction guard (operator, 2026-07-16).** The
master keeps N same-project goals straight by NOT keeping them straight in context: judgment is leased one
pre-scoped item at a time (item carries slug, evidence, checklist, bounded tail — handle, resolve, forget);
`epic-<slug>` is the universal join key; quiet epics never enter context (304 + `on_task` rollup). Two
digest additions: (a) `EpicDigest.recent_interventions[]` — the last ~3 ledgered interventions on this epic
(actor, timestamp, payload summary) so a post-`/clear` master neither repeats nor contradicts its own prior
steer (read from the ledger, no new table); (b) the supervise-epics skill instructs explicitly: handle items
one at a time, do not build a mental fleet model — the board holds it. Optional at scale: multiple masters
scoped per mission (the `supervisors.repos` scoping generalizes to missions) — not needed below ~10
concurrent epics. *Phase 6 (digest field) + Phase 9 (skill text).* 

**15.12 Epic queue, priority, and dependency dispatch (operator, 2026-07-16).** Activates the reserved
`epics.state='pending'`. (a) `flowbee epic queue <file>` registers without launching (validated, scope
declared). (b) Frontmatter `blocked_by: [<slug>…]` — a dependency is satisfied when the blocker's PR
**MERGES to main** (reconciled fact; never the session's `State: done` claim — dependents cut from main and
need the code actually there). (c) Frontmatter `priority: 1..10`, lower=urgent (0021 convention), with
scheduler-style aging so low priority never starves; deps dominate, priority orders only the eligible set;
`flowbee epic reprioritize` adjusts it post-registration (operational, not part of the immutable spec).
(d) A deterministic DISPATCHER in the consolidated supervision ticker: on every capacity event (epic
terminal, host freed, account under ceiling, dependency merged) compute eligible = deps-merged ∧ scope
disjoint from ACTIVE epics ∧ weekly-headroom account ∧ free matching host; order by aged priority;
auto-launch through the existing gated launch path. Queued (not active) epics may overlap in scope freely —
overlap is an ordering constraint (implicit serialization), not an authoring error. (e) Blocker fails
(abandoned/denied/needs-human) → dependents hold `pending` + a `dep_failed` attention item (master:
re-plan / drop / force); prolonged ineligibility surfaces as low-priority attention with the concrete
reason. (f) `flowbee epic plan` renders the queue in eligibility order with each epic's specific blocker
("waiting on merge of X" / "scope busy until Y" / "no headroom until <resets_at>" / "no free host").
*Phase 9 (dispatcher + queue CLI; `dep_failed` kind lands with the Phase 5 enum), placement view extends §4.5.*

**15.13 The SEAT registry — launch provisions sessions from registered account×box seats (operator,
2026-07-16).** A **seat** = (account, box, agent family, config dir/env). The same account logged in on two
boxes is two seats sharing ONE quota bucket (accountUuid); quota keys on the account, occupancy on the box.
Flowbee NEVER performs logins — the human authenticates each account on each box once; the registry records
where each account is already usable, and `auth_dead` routes re-login back to the human. (a) Schema: `seats`
(id, box → epic_hosts, agent_family, account_key → account_windows, config_dir/codex_home, extra env,
enabled, health: ready|limit_critical|auth_dead|unreachable, last_probe_at) — folded into migration 0028.
(b) Registration: `flowbee seat add`, plus `flowbee seat discover <box>` — acctprobe over ssh scans the
box's home for .claude* dirs / CODEX_HOME, resolves accountUuid/account_id + live limits, proposes seats
for confirmation. (c) LAUNCH = SEAT SELECTION: the dispatcher/launch gate picks a ready seat matching the
epic's `agent:` family with weekly headroom on a free box, and injects the seat's env
(CLAUDE_CONFIG_DIR/CODEX_HOME + FLOWBEE_ACCOUNT) at tmux-session creation — the epic lane PROVISIONS
sessions on demand (Phase 2's LaunchEpicSession, now fed by the registry); adoption of pre-existing
sessions remains only for masters and legacy panes. (d) Seat health: the staggered capacity ticker probes
seats (acctprobe over ssh), feeding the dashboard capacity strip as an account×box seat matrix with limits,
health, staleness badges, and current occupant epic. Supersedes the bare `--host`/frontmatter-host flow
(kept as an override that must name a registered seat or box). *Phase 6 (schema, discovery, launch gate);
dashboard strip Phase 7.*

**15.14 Per-epic visual explainer, rendered in the dashboard (operator, 2026-07-16).** Every epic maintains
`epics/<slug>-explainer.html` on its branch — a self-contained HTML page (mermaid diagrams + prose, built per
the visual-explainer method, vendored at `docs/skills/visual-explainer/` so BOTH Claude and Codex runners can
follow it) that communicates what the epic is building and where it stands. Contract: authored with the spec
(plan-of-record: architecture + step-flow diagram), refreshed at each step completion (progress, discoveries,
plan deviations), finalized at finish (the as-built story — also handed to the cross-family reviewer as
context). `## Status` stays the machine-readable truth; the explainer is the human rendering — never parsed
by the control plane. Dashboard: the epic drawer gains an Explainer tab — flowbee reads the file off the epic
branch via the mirror (same path as status ingestion) and serves it in a HARD-SANDBOXED iframe
(`sandbox="allow-scripts"`, NO allow-same-origin, strict CSP; agent-authored HTML gets zero access to
dashboard origin/cookies/API) with a staleness badge when the explainer's last commit lags status updates.
Ingestion auto-registers it as the first standard `artifacts` kind (explainer). The epic's own explainer file
is implicitly in scope (same F1 treatment as the epic .md). *Phase 4 (contract bullet + skill vendoring),
Phase 7 (drawer tab + sandboxed serving), Phase 9 (artifact registration).*

**15.15 The LAUNCH LADDER — supervised staged launch through a local pane (operator, 2026-07-16; DELIBERATELY
REVERSES Phase 2's remote-tmux/BatchMode-only model).** Flowbee launches an epic by driving a LOCAL tmux
session (named `epic-<slug>`, on the control-plane box — the operator's single attachable pane of glass)
through a verified stage machine, each stage pane-classified before the next fires, composed entirely from
merged tmuxio primitives (NewSession + verified Send + Classify):
1. create local session `epic-<slug>`;
2. (remote seat) send `ssh -t <user>@<box> tmux new -A -s epic-<slug>` — the agent lives in a REMOTE tmux
   that survives disconnects/control-plane restarts; the local pane is only the attachment, which flowbee
   re-establishes automatically if the pipe drops (watchdog heals attach; the agent never notices). An
   INTERACTIVE auth prompt (password/2FA) at this stage classifies AWAITING_INPUT → a launch-stage attention
   item routed to the human, who answers once in the pane — tolerated, not a BatchMode hard-fail;
3. verify remote shell arrival (prompt classification, timeout);
4. send the seat's CLI launch line (env + binary, e.g. `CLAUDE_CONFIG_DIR=~/.claude-pearl claude` or the
   codex equivalent — built from the registered seat, closed-verb discipline);
5. classify until IDLE_AT_PROMPT = CLI up and healthy (timeout);
6. verified-send the goal prompt (family verb table Launch template);
7. confirm WORKING → state running.
Any stage timeout/misclassification → `launch_failed` attention item + rollback (kill local session, release
seat/host/scope via the launching-reaper path). Local seats run the same ladder minus stages 2-3. FALLBACK
CONTROL PATH: `goal_sessions.box` still records the box, so per-op `ssh -- <box> tmux ...` (the Phase-2
remoteWrap machinery, retained) can capture/send directly against the remote tmux if the local attachment
pane is lost — dual-path supervision. tmuxio needs no new transport API (the reviewer-endorsed deletion of
SessionSpec.RemoteHost stands; the ladder is orchestration, not transport). *Phase 6 (launch-gate rewrite,
replacing LaunchEpicSession's one-shot), attach-healing in the consolidated ticker.*

**15.16 External status consumers are first-class (operator, 2026-07-16).** A Stream Deck plugin
(dev/flowbee-elgato) mirrors flowbee status to physical keys; more consumers will follow. The Phase 6/7
read-models (`/v1/epics/digest`, `/v1/board`, the `epics` SSE topic, the capacity strip) are therefore a
PUBLIC, versioned contract, not dashboard internals: stable field names, small payloads (support a
`?fields=` projection and/or a counts-only summary endpoint for constrained consumers), token auth matching
the existing worker-token scheme, and SSE consumable outside the dashboard origin. The elgato session's
needs (full doc: ~/Desktop/flowbee-elgato-api-needs.md) are ACCEPTED into the Phase 6/7 contract:
(a) `EpicDigest` carries the tmux jump target EXPLICITLY — `tmux` session name + `host` ('' = control-plane
box) — never joined by slug convention (with the §15.15 launch ladder, the local attachment pane IS the
iTerm-focus target even for remote epics); (b) SSE hygiene: keepalive comment every 15-30s (half-open
sockets on slept laptops otherwise go silently stale), named `event:` per topic, `digest_seq` in every
payload for client dedupe — SSE is a lossy nudge, poll is truth; (c) counts-only `GET /v1/summary` with
ETag/304: {digest_seq, attention_total + by_priority, epics_blocked/on_task, dispatch_paused, stranded,
worst_account_severity}; (d) JSON contract: `[]` never null, stable ordering (accounts by account_key),
account read-model includes resets_at; (e) open-tier loopback reads stay open; GET /v1/control likely joins
the open loopback tier (small posture decision at Phase 6 wiring). NOTE: the elgato session independently
added an open-read GET /v1/sessions (ListGoalSessions) to a flowbee checkout — locate and reconcile that
commit through the same review train before Phase 6 lands overlapping surface. *Phases 6-7.*

**15.9 Deterministic classifiers stay conservative.** The infra-flake and drift pattern registries only
auto-act on well-known signatures; anything ambiguous routes to the master. A flake with no clean signature
(passed-in-isolation/failed-under-shard) must NOT match — landing on the master is the correct behavior.
Auto-rerun budgets stay small. *Phases 6/8 (registries).*
