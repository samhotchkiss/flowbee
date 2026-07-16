# Epic runner contract — flowbee

You are executing an epic spec autonomously, unattended, for hours to days.
This file is your standing contract. The epic file is your task. Nothing
else is in scope.

## Branch

One branch per epic: `epic/<slug>` cut from current main at start. Never
force-push. Never rebase — full stop, even to catch up with main.

If main moves under you and a step genuinely needs it, integrating main is
allowed ONLY when a `## Amendments` entry on this branch explicitly instructs
it, and ONLY as `git merge origin/main` (a merge commit) — never a rebase. The
never-rebase rule stands no matter what an amendment says: an amendment can
authorize a merge-of-main, never a history rewrite.

## Work order

Work `## Steps` in order, top to bottom. A step is done only when its
`Validate:` command actually ran, in this checkout, and passed — not when
the code "looks right." Record the evidence (the command and its result)
in `## Status` before moving to the next step.

## Status discipline

Update `## Status` whenever state changes: step start, step finish, a
blocker appears, a blocker clears, evidence recorded. Don't batch updates
for later — the dashboard reads the branch every ~2 minutes and an operator may be
looking. Fields:

```
## Status
Updated: <ISO timestamp> · Current: step N/M · State: building|blocked|done
- [x] Step 1 — <criterion> (evidence: <command> → <result>)
- [ ] Step 2 — <criterion>
Blockers: <none, or what's stopping you and what you need>
```

Liveness is monitored separately (heartbeat) — `## Status` is for MEANING,
not a keepalive ping. Don't touch it just to bump the timestamp.

## Commits

Commit at natural boundaries, not one giant commit at the end. Every
step-completion commit carries the trailer:

```
Epic-Step: N/M — <short criterion>
```

## Push and the draft PR

Push every step-completion commit as you make it — one push per completion,
don't batch pushes for the end. CI runs per push, so a red result localizes to
the step that broke it instead of surfacing as one opaque failure at hour 40.

Open the PR as a DRAFT right after your first step-completion push, from
`epic/<slug>`, titled with the epic's title. Keep pushing step commits onto it;
CI re-runs on each push and Flowbee reads the result per head. Leave it a draft
the whole way — a draft PR is the running CI surface, not a request for review.
Do NOT mark it ready or label it `needs-claude` while steps remain (that
happens once, at finish).

## Scope

Never touch a path outside the epic's declared `scope:` globs. If a step
needs a path outside scope, that's a blocker: stop, record it in `## Status`
Blockers (what you need and why), and HALT the epic — keep the session alive
(liveness is watched separately; an operator reads Blockers off the dashboard
and unblocks you). Do NOT reorder the remaining steps to stay busy: steps
execute in order, and a linear, reviewable PR story matters more than
utilization.

The ONE bounded exception: you may proceed to a later step ONLY if it has no
dependency whatsoever on the blocked step's outcome — no shared files, no
build/test dependency on its changes, no ordering assumption in the spec.
When you take this exception, state that reasoning explicitly in `## Status`
next to the blocker entry. If you cannot state it in one sentence, it isn't
independent — halt instead. Do not silently widen scope.

## flowbee specifics

- Go control-plane repo. Before the final PR, run the full suite:
  `go test ./...` — this must include `test/acceptance`, not just unit
  packages. A step that only ran unit tests is not validated.
- Migrations: never renumber or reuse a filename already applied anywhere
  (main or another live epic branch). New migrations take the next free
  number after what's on main when you write them — check main right
  before adding one, since another epic may have taken a number meanwhile.
- Never print, log, cat, or echo `serve.env` or `fleet.env` — secrets. If a
  step seems to require reading one, that's a blocker, not a workaround.

## Finish

Full `go test ./...` (including `test/acceptance`) green. Finalize
`## Status`: `State: done`, every checklist box checked with its evidence.
Then take the DRAFT PR you opened early (see Push) to completion: push the
final commits, fill in the body linking the epic file and summarizing what
shipped, mark it READY FOR REVIEW, and label it `needs-claude`. It is exactly
one PR per epic — the same one you opened as a draft, now ready — never a
second PR. Then stop — do not keep working after the PR is marked ready.

## Escalation

Blocked more than 2 hours on something outside your control (auth, infra,
account quota, a flaky upstream dependency) — record it in `## Status`
Blockers with what you tried and what you need, and keep the session alive.
The watchdog handles resume; an operator reads Blockers off the dashboard
and unblocks you. Don't spin retrying the same failing command for hours.
