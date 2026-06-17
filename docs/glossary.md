# Glossary

This is the canonical list of Flowbee's domain terms. Each entry is written to
be understood on its own, so a new contributor can read a review, issue, or
design note without first reading the source. Terms are ordered alphabetically.

### anti-affinity

A placement rule that deliberately keeps certain actors or work apart so they
are not co-located. Flowbee uses it for identity diversity: for example, a
build and its review should not be assigned to the same agent (or the same
model family), so a reviewer can't rubber-stamp its own work or a shared blind
spot. Anti-affinity is expressed as a constraint when a job is leased.

### barrier

A synchronization point that gates progress until a set of conditions is met.
Downstream work is held at the barrier until every prerequisite — for instance,
all build-review verdicts for an issue, or all issues in an epic — has reached
the required state. Barriers are how Flowbee fans work out and then waits for it
to converge before advancing.

### content-integrity gate

A verification check that confirms the content of a change is correct and
untampered before an operation is allowed to proceed. In Flowbee it compares the
diff that was actually produced against what was approved (e.g. via hash), so a
prompt-injected or drifted change cannot slip into `main`. The gate is a
precondition for autonomous merge.

### epic

A top-level unit of work that groups related work together. An epic carries a
goal and a set of issues with their dependencies; Flowbee breaks the goal down,
drives each issue through the pipeline, and tracks the epic as the umbrella that
contains them. It is the largest planning unit a user or planner agent submits.

### epoch

A monotonically advancing generation counter used to distinguish eras of state.
When a job is reassigned or a claim is refreshed, the epoch increments, so the
system can tell current state from stale state. Epochs are the basis of fencing:
an action stamped with an old epoch is recognized as out of date.

### fence

A guard — typically a token or value such as an epoch — that rejects stale
actors so only the current owner can act. If a slow or zombie worker tries to
report on a job after it has been reassigned, the fence detects the mismatched
token and refuses the write. Fencing is what makes Flowbee's leases safe under
reassignment.

### lease

A time-bounded, renewable claim of ownership over a job or resource. A worker
leases a job, holds it while doing the work, and renews the lease to keep it;
if the lease expires without renewal, the job becomes available to another
worker. Leases are fenced (see **fence**) so exactly one worker owns a job at a
time.

### reconcile-in / project-out

The paired directions of state flow between Flowbee and the outside world.
**Reconcile-in** pulls external/desired facts *into* Flowbee — for example,
reading GitHub for whether a PR exists, CI status, or a merge — and folds them
into Flowbee's own state. **Project-out** pushes Flowbee's authoritative state
*out* to consumers — for example, opening or updating issues and PRs on GitHub
to render the current process state. Flowbee is the system of record; GitHub is
ground truth only for the facts it owns.

### supersede

To replace an older item or version with a newer one and mark the old one as no
longer authoritative. When a spec, plan, or job is regenerated, the prior
version is superseded so consumers follow the current one. Superseding records
lineage without deleting history.
