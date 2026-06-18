# Flowbee Pipeline Stages

Flowbee moves a unit of work from intent to a merged PR through the stages below. Every
stage that does real work is executed by a **real agent** run by the worker harness — the
control plane only schedules, reconciles GitHub facts, and merges.

Work enters Flowbee in one of two ways: label a GitHub issue `flowbee:build`, or `POST` a work item to `/v1/specs`.

## Two entry points

- **Labeled GitHub issue → build.** An issue carrying the `flowbee:build` label already
  contains its own spec (the body is parsed into task / spec / acceptance), so intake
  adopts it directly at the `ready` (build) stage. No spec authoring is needed.
- **An idea → the spec flow.** Ingested via `POST /v1/specs`, an idea enters at
  `spec_authoring` so an agent drafts the spec before any code is written.

## The stages, in order

1. **spec_authoring** — a `spec_author` agent drafts the specification of what to build.
2. **spec_review** — a `spec_reviewer` agent checks the draft (style + requirements) and
   either signs it off, amends it in place, or sends it back for design. Flowbee — never
   the worker — commits the approved/amended spec bytes (content-hash bound).
3. **ready** — the approved work is queued, waiting for an engineering worker. (Labeled
   issues enter here.)
4. **building** — an `eng_worker` agent implements the change on the issue branch
   `flowbee/issue-N`, committing its work with a detailed message.
5. **review_pending** — the change is queued for code review; its required capability
   flips to the reviewer role so an independent reviewer (never the builder, never the
   same model family — §5.5 anti-affinity) claims it.
6. **code_review** — a `code_reviewer` agent rebases onto `main`, examines the diff
   against the spec, and lands an **empty** `review(<id>): APPROVED | CHANGES REQUESTED`
   commit plus a findings comment on the issue. Its verdict is a *claim* only — the gate
   mints the authoritative, SHA-bound verdict from reconciled facts.
7. **mergeable** — the verdict is bound to the reconciled head/base SHA and CI is green.
8. **merging** — the control plane (the sole merger) merges via a **merge commit** so the
   full per-node trail stays reachable and `git log --first-parent main` is clean.
9. **done** — merged. A post-merge archive commit records the run; sibling jobs have their
   `base_sha` refreshed to the new `main` so they build on current code.

## What keeps it honest

- **CHANGES REQUESTED** bounces the job back to `building` (a rebuild) with the reviewer's
  findings fed to the agent; after `max_bounces` it escalates to `needs_human`.
- A **forward-progress watchdog** guarantees no job wedges (see [operating.md](operating.md)).
- The **git history is the record**: per-issue branch, per-node commits, reviewer empty
  verdict commit, merge commit — you can read exactly how a change came to be.

See [operating.md](operating.md) for the full stand-up-and-run runbook.
