# Flowbee FAQ

Short answers to the questions new operators ask first. For the full runbook see
[`operating.md`](operating.md); for the system map see
[`architecture-overview.md`](architecture-overview.md).

---

### What GitHub token permissions does Flowbee need?

Flowbee requires a **fine-grained personal access token (PAT)** with the following
repository-scoped write permissions for each repo it manages:

- **Contents** — to push branches and read code
- **Pull requests** — to open, update, and merge PRs
- **Issues** — to create and close issues that represent jobs

Set the token in the `FLOWBEE_GITHUB_TOKEN` environment variable. The **control plane is
the sole GitHub API caller** — workers commit and push via git over SSH using the box's own
key, never touching the GitHub REST API directly. Scope the token to only the repos listed
under `repos:` in `flowbee.yaml`; no org-level or admin permissions are required.

---

### Does Flowbee need a database server?

No — all state lives in a single SQLite file (`flowbee.db`) in WAL mode; there is no
Postgres/MySQL to run. It is litestream-friendly for continuous backup to object storage.

---

### Does Flowbee run as a managed service?

Yes — both halves run under systemd with `Restart=always`: `flowbee serve --systemd` for
the control plane and `flowbee fleet --systemd` for each worker box, so they survive
reboots and restart cleanly.

---

### What happens when two Flowbee PRs conflict?

When one PR merges and a sibling no longer applies cleanly, Flowbee routes the sibling to
a `conflict_resolver` worker that rebases onto current `main` and resolves the markers; the
resolved diff is then re-reviewed and merged — conflicts resolve autonomously instead of
escalating to a human.

---

### What happens to the per-issue branch after a change merges?

Flowbee deletes the `flowbee/issue-N` branch automatically after the merge — the merge
commit keeps the branch's commits reachable from main, so only the ref is removed, and the
repo does not accumulate stale branches.

---

### What models does Flowbee use for building versus reviewing?

By default, builders and the spec author run Sonnet (`claude --model sonnet`); the code
reviewer, spec reviewer, and conflict resolver run Opus (`claude --model opus`). The
reviewer never shares the builder model, so reviews are uncorrelated with the code that
produced them (§5.5).

---

### How do I pause Flowbee without losing state?

Stop the fleet (so no new work is claimed by workers) while leaving the control plane
running, or stop both the fleet and the control plane entirely — either way, nothing is
lost. All persistent state lives in the ledger (`flowbee.db`) and in GitHub. On restart,
the next reconcile re-derives the full world from those two sources, exactly as if the
process had never stopped. There is no in-memory state to drain or checkpoint before
shutting down.

---

### Can one Flowbee control plane manage multiple repos?

Yes — list each repo under `repos:` in `flowbee.yaml`; one control plane runs a per-repo
reconcile/project loop over a shared, repo-agnostic worker fleet with a global scheduler.

---

### What is the difference between `flowbee fleet` and `flowbee up`?

`flowbee up` is the single-box all-in-one that starts the control plane plus one worker per
role for local use; `flowbee fleet` brings up just the worker roles on a box and connects to
a separate `flowbee serve` control plane (the multi-box topology).

---

### How do I check which build of Flowbee is running?

Run `flowbee version`. It prints the embedded git SHA recorded at build time via
`debug.ReadBuildInfo`, so you can pin the exact commit without consulting the host
environment. The control plane also logs the build SHA on its startup line, so the
same information is available in whatever log aggregator you point at the process.

---

### What does "reconcile-first" mean, and why does GitHub stay the source of truth?

**Reconcile-first** means Flowbee never trusts its own memory over the world. On every
sweep the control plane re-derives the state of each job from **GitHub + the ledger**
rather than from anything held in RAM. There is no hidden in-memory state to lose: kill
the control plane mid-flight and the next reconcile rebuilds the same picture.

GitHub stays ground truth for the facts it owns — a PR exists, CI is green, a branch
merged — because those facts are observed, not asserted. The control plane is the *only*
component that calls the GitHub API and the *only* one that merges; workers only ever
commit and push their own branches. That split (desired state in the control plane,
observed state from GitHub and the workers) is what lets any process die and recover
without a coordinator to nurse it back. A GitHub issue or PR is a *rendering* of a
Flowbee job, but the underlying facts about that PR live where they can't drift.

---

### Why is there no LLM in the control plane?

So the orchestrator can be **deterministic and replayable**. The control plane makes
every scheduling, ownership, and merge decision, and its entire history folds from an
event log — so it never hallucinates, it's unit-tested, and a given input always
produces the same decision. An LLM in that path would make the brain non-replayable and
unauditable.

The intelligence lives at the **edges**, in the agents the workers wrap (Codex, Claude,
a local model). Roles are config: one model builds, a distinct-lens model reviews, swap
either freely. Keeping the judgement in the agents and the bookkeeping in a deterministic
core is the whole design — the core you can trust to be correct, the edges you can swap
to be smart.

---

### How does autonomous self-merge gating work (CI + content-integrity + SHA-bound verdict)?

With `FLOWBEE_ALLOW_SELF_MERGE=1` (or `flowbee up --self-merge`), an approved change can
merge to `main` with no human gate — but only when **three** conditions hold at once:

1. **CI is green** on the reconciled head.
2. The change passes the **content-integrity gate** — the admission check that keeps a
   tampered or prompt-injected diff out of `main`. Verdicts derive from reconciled facts,
   never a worker's say-so.
3. The reviewer's **verdict binds to the reconciled head/base SHA**. The approval is for
   *that specific diff*, not "this branch" in the abstract.

That third point is why **you must not push to a repo's `main` while one of its issues is
in review.** Moving the head supersedes the SHA-bound verdict — the approval no longer
matches what would merge — so the merge falls back to a human (`merge_handoff`). That is
correct safety behavior, not a bug. Every write that takes effect, the merge included,
passes through the single gate that enforces both the lease/epoch fence and these
admission invariants.

---

### How do I bring up a worker fleet, and what is auto-respawn?

One command per worker box brings up every role — build workers, a code reviewer, a spec
author, and a spec reviewer:

```sh
FLOWBEE_REPO_URL=git@github.com:samhotchkiss/flowbee.git \
flowbee fleet --url http://<control-plane-host>:7070 --builders 3
```

It smoke-tests the agent CLI first (fail loud, not mid-job), then spawns `--builders` N
parallel build workers plus one each of the review/author roles. Workers commit and push
with the box's own key; the control plane never gets their creds. Run it under
`--systemd` so the fleet survives reboots.

**Auto-respawn** is the loop staying up under churn: a worker is a thin, self-identifying
pull-loop, so when one finishes (or a job's agent process exits) the loop dials back out
for the next lease rather than tearing the fleet down. Paired with the control plane's
watchdogs — the forward-progress watchdog re-folds stuck jobs every 60s, and
`stranded: true` in `GET /v1/fleet-health` is the loud "no live worker" signal — a fleet
keeps itself fed without hand-holding. If a box does go down entirely, restart
`flowbee fleet` on it and its leases re-arm with a fresh epoch.

---

### What happens if a reviewer keeps rejecting the same task?

After **6 changes-requested rejections by the same review node**, Flowbee parks the job
for human intervention with escalation reason `reviewer_rejections`. The job moves to
`needs_human` so an operator can inspect the disagreement between builder and reviewer
before more cycles are spent.

This per-reviewer cap is distinct from — and fires before — the cruder `max_bounces`
backstop, which counts total rejections across **all** reviewers combined. You can think of
them as two separate circuit breakers: `reviewer_rejections` catches the case where one
particular reviewer is consistently unhappy with the work; `max_bounces` catches the case
where the job has accumulated too many total review trips regardless of which node did the
rejecting. Both land the job in `needs_human`; use `flowbee requeue <job-id>` once the
underlying issue is resolved to re-arm it with a fresh budget.

---

### Why might a change to Flowbee's own source require a human to merge?

Because a change to Flowbee can change the very rules that would approve it. A diff that
touches the orchestrator's own decision logic, the gate, or the merge path can't be
allowed to wave itself through — the safe default is to escalate self-modifying changes to
`needs_human` so a person reviews the change to the rules before those rules govern
anything else.

The same handoff happens for ordinary safety reasons: a verdict whose SHA no longer binds
(someone pushed to `main` mid-review), a job that bounced CI past `max_bounces`, or a
no-eligible-worker dead-end all land in `needs_human` or `merge_handoff`. Fix the cause
and `flowbee requeue <job-id>` to re-arm it with a fresh budget. A human in the loop here
is the system working as designed, not failing.

---

### What database does Flowbee use?

Flowbee uses a single SQLite file (`flowbee.db`) in WAL mode — there is no database server
to run or manage.

---

### Can Flowbee decompose a goal into multiple issues?

Yes. POST `/v1/epics` with a `goal` string and an `issues` list. Flowbee first runs a
barrier issue-review step that validates the decomposition before any work begins; once
that passes, each issue fans out independently through the normal
spec → build → review → merge flow.

---
