# Autonomous self-clearing ladder

Flowbee's design goal is that **once you create an issue, the system drives it to
completion itself** — no operator has to babysit the board, run `flowbee requeue`, or
merge PRs by hand. Historically `needs_human` was a pure sink: ~8 escalation triggers
converged there and the only exit was a human. The self-clearing ladder replaces
"escalate to a human" with "escalate to a **stronger autonomous strategy**", so the only
terminal states become `done` (it shipped) and `cancelled` (the system tried hard and,
with a written trail, decided it could not land it).

## The ladder

A stuck job climbs this ladder; each rung is a genuinely different, more capable attempt,
and every rung is bounded so nothing loops forever.

| # | Trigger | Autonomous response | Where |
|---|---------|---------------------|-------|
| 0 | `stall` (worker went quiet) | **Mechanical janitor** re-queues it — cheap, no LLM. Correlated-failure breaker + per-job cap + SHA-progress keep it bounded. | `Store.JanitorUnblock` |
| 1 | `bounces` / `attempts` / `reviewer_rejections` (repeated failure) | **Advisor** (read-only LLM) reads the actual review findings / CI failures and re-arms with a concrete correction + a **fresh budget**. A blind retry re-fails; a guided one lands. | `internal/advisor`, `Store.ApplyAdvisorVerdict` |
| 2 | Advisor consulted its per-job cap and still failing, **or** parked idle past 24h | **Auto-cancel** with the full ledger trail as the post-mortem. The board self-clears; `flowbee requeue` reopens it. | `Store.AutoCancelExhausted` |
| — | PR approved but **can't merge** (head moved after review, unverifiable merge) | **Merge-fixer** re-arms it to a fixer worker: "fetch the branch, rebase onto main, resolve conflicts, fix failing checks, make it mergeable." The normal merge gate then ships it. | `Store.EscalateStuckMergeHandoff` |
| — | Agent blocks on an interactive prompt | **Elicitation fail-fast**: report `awaiting_input`, clean re-dispatch in one heartbeat instead of the ~4-min stale reap. | `internal/worker` `promptDetector` |

**Never auto-retried** (these genuinely need external action, not a blind retry, and stay
parked): `project_out` (a permanent GitHub 4xx — fix the GitHub state), `pr_closed` (a
human closed it), `cost` (over the $ ceiling — raise it), `needs_design` (a deliberate
product decision).

## The one remaining human touch

**flowbee merging its own source.** A bad self-merge can break the orchestrator itself,
after which nothing can auto-recover it. So a `merge_handoff` whose reason is a
source/denylist hit is excluded from the merge-fixer (an allowlist, not a denylist — an
unknown reason also stays parked) and waits for you. Everything in every other repo is
fully autonomous. If you want to lift even this, that's a deliberate change (ideally with a
post-merge health-check + auto-revert net).

## Turning it on

The ladder is **opt-in** — it makes LLM calls (the advisor needs an agent CLI on the serve
box) and can cancel/re-drive work, so it doesn't switch on silently. One master flag turns
on the whole thing:

```
FLOWBEE_AUTONOMOUS=on        # advisor + auto-cancel backstop + merge-fixer
```

Or enable the rungs individually (each also acts as an override of the master switch):

| Flag | Default | Effect |
|------|---------|--------|
| `FLOWBEE_SELF_UNBLOCK` | **on** | The mechanical `stall` janitor (Rung 0). Set `off` to disable. |
| `FLOWBEE_ADVISOR` | off | The Rung-1 advisor. Requires an agent CLI. |
| `FLOWBEE_ADVISOR_CMD` | `claude -p …` | Override the advisor CLI (set the `codex exec` form on a codex box). |
| `FLOWBEE_AUTO_CANCEL_EXHAUSTED` | off | The Rung-2 terminal backstop (auto-cancel). |
| `FLOWBEE_MERGE_FIXER` | off | The merge-fixer for un-mergeable PRs. |
| `FLOWBEE_ELICITATION_FAILFAST` | **on** | Worker-side stuck-prompt detection. Set `off` to disable. |

Each rung is bounded (per-job caps, cooldowns, a correlated-failure breaker, a 24h time
backstop) so enabling the ladder cannot fan out into a runaway loop, and every action is
event-sourced (the ledger is the audit trail and every cancel is reversible via requeue).
