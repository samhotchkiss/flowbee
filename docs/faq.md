# Flowbee FAQ

Short answers to the questions new operators ask first. For the full runbook see
[`operating.md`](operating.md); for the system map see
[`architecture-overview.md`](architecture-overview.md).

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
