# Glossary
> Flowbee terms, operator-oriented.

This is the canonical list of Flowbee's domain vocabulary. These terms recur
throughout the design docs, the issue/PR discussions, and the code; this page
gives each one a single, linkable definition so contributors and reviewers
share the same meaning. Terms are ordered alphabetically.

### anti-affinity

A placement rule that keeps certain actors *apart* so that independence is
structural rather than advisory. Flowbee enforces anti-affinity at lease time:
the scheduler refuses to hand a job to a worker that would violate it. The
canonical constraints keep a reviewer distinct from the thing it reviews — e.g.
the build worker's identity *and* model family must differ from the code
reviewer's, the spec author's lens must differ from the spec reviewer's, and the
code reviewer must differ from the merger. This is how Flowbee prevents an actor
from rubber-stamping its own work or a same-family pair from sharing a blind
spot.

### barrier

A synchronization gate that holds a group of work back until a condition is met.
The primary example is the **epic**-level review barrier: a single review job
covering an entire epic. None of the epic's child issues may fan out or be
scheduled until that barrier passes (the epic's spec review is signed off). A
barrier turns "review the whole plan first" into an enforced ordering rather
than a convention.

### base_sha

**base_sha** — The commit on the integration branch from which a build is cut. After a sibling branch merges, `base_sha` is refreshed to point at the current tip of `main`, ensuring subsequent builds start from the latest integrated state.

### content-integrity gate

A deterministic, non-LLM safety check that runs over a change's diff before it
is allowed to merge. It is admission control standing between an untrusted
worker's patch and `main`: it enforces a path denylist (protected files such as
CI config can't be touched), checks that the diff stays within its declared
blast radius, and runs static checks. Because it is deterministic, it keeps a
prompt-injected or tampered diff out of `main` without relying on an agent's
judgment.

### epic

A top-level unit of work that groups related issues under one goal. An epic goes
through a collective review **barrier** before its children are released: while
the epic-level review is pending, the child issues sit in the backlog (tracked
but unscheduled), and only once the epic is signed off do they fan out for
individual building and review. An epic also anchors shared state such as its
review verdict and identity bindings.

### epoch

A monotonically increasing generation counter used to distinguish eras of a
job's ownership. Every time a job's **lease** is revoked or reassigned, its
epoch is bumped. The epoch is the value carried by the **fence**: a write
arriving with a stale (lower) epoch is rejected, so a previous owner's
operations can never apply to the current era.

### fence

A guard that rejects stale actors so only the current owner of a job can act on
it. In Flowbee the fencing token is the **epoch**: when a lease is reassigned the
epoch advances, and a zombie worker still holding the old epoch is "fenced out" —
its write-back is refused (409 Conflict). Fencing is what makes leases
exactly-once: a slow or network-partitioned worker cannot clobber a job that has
since been handed to someone else.

### lease

A time-bounded, renewable claim of ownership over a job. A worker dials out to
the control plane, leases a job for the duration of its TTL, executes it with
whatever agent it wraps, and reports the result; it renews the lease while still
working and the lease expires if it goes silent. Each lease grant is assigned a monotonically increasing **epoch** used as the fencing token for that ownership period. Each lease is fenced by an
**epoch** so that exactly one worker holds a given job at a time.

### lease reaping

When a worker stops heartbeating — due to a crash, OOM, or network loss — the
control plane reaps its **lease** fast, after a few missed heartbeats, presuming
the worker dead. Reaping revokes the lease, bumps the **epoch** to fence out the
old worker if it resurfaces, and re-arms the job so another worker can claim it.
Fast reaping (rather than waiting for the full TTL to expire) keeps queue latency
low when a worker dies mid-job.

### merge queue

The ordered line of approved PRs waiting to land on `main`. Flowbee runs a
merge queue with a batch size of one: it integrates exactly one PR at a time,
retrying a transient not-mergeable state before routing to the
**conflict_resolver**. Keeping the batch size at one maximises isolation — each
integration is a single, attributable change — and the retry avoids opening a
conflict-resolution issue for a race condition that resolves on its own within
seconds.

### reconcile-in / project-out

The paired directions of state flow at the Flowbee ↔ GitHub boundary, with the
control plane sitting at the hinge between them. **reconcile-in** is the inbound
direction: observed facts — a worker's result, a PR's existence, CI status — flow
*in* and the control plane reconciles actual state against desired state.
**project-out** is the outbound direction: the control plane's decisions and
desired state are projected *out* to GitHub and workers (labels, status checks,
merges) through an idempotent outbox. Reconcile-in is "learn what is true";
project-out is "make the world match the decision."

### supersede

To replace an older item or verdict with a newer one, marking the old as no
longer authoritative. In Flowbee a sign-off is bound to the content it approved,
so when that content changes the prior verdict is *superseded* and the relevant
gate re-arms for re-evaluation — for example, a new base/head SHA invalidates a
SHA-bound review and CI verdict, and a changed spec content-hash invalidates a
spec sign-off. A superseded state is non-terminal: the job re-arms from it rather
than ending.

### supersede vs. fence

These are easy to confuse but distinct: a **fence** rejects a *stale actor*
(wrong epoch) acting on otherwise-current state, while **supersede** invalidates
a *stale verdict* because the underlying content moved. Fencing protects
ownership; superseding protects correctness of decisions.
