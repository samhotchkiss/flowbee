# Glossary

This glossary defines the core Flowbee terms used across the README, architecture
notes, and design docs. Treat it as the shared vocabulary for contributors reading
or reviewing Flowbee work.

### Anti-affinity

Anti-affinity is the scheduling rule that keeps related responsibilities apart.
For example, the worker that builds a change must not also review it, and a
reviewer must not come from the same model family when that would weaken the
independence of the review.

### Barrier

A barrier is a synchronization point that stops progress until required
conditions are satisfied. In Flowbee, gates act as barriers: work cannot advance
until the relevant verdicts, checks, or reconciled facts are present.

### Content-integrity gate

The content-integrity gate is the deterministic admission check for returned
diffs before they can be promoted toward merge. It treats worker output as
untrusted data and checks the change's paths, blast radius, and static properties
so privileged or tampered content cannot move forward on an agent verdict alone.

### Epoch

An epoch is the monotonically increasing generation number carried by a lease.
Every write includes the epoch it believes is current; writes with stale epochs
are rejected, which prevents an old worker from overwriting newer work after a
lease has moved on.

### Epic

An epic is a top-level unit of work that groups related issues and dependencies
under one goal. Flowbee can review an epic as a whole before sending individual
work items through build, review, and merge.

### Fence

A fence is the guard value that blocks stale actors from writing. Flowbee uses
the lease epoch as a fencing token, and also binds verdicts to reconciled GitHub
state such as the current head and base SHAs.

### Lease

A lease is a time-bounded, renewable claim that gives one worker ownership of a
job or stage. The control plane grants the lease, tracks heartbeats, and rejects
late heartbeats or results when the lease's epoch is no longer current.

### Reconcile-in / project-out

Reconcile-in is the inward flow of observed facts into Flowbee, such as worker
results, GitHub state, or CI status. Project-out is the outward flow of Flowbee's
decisions to workers and external systems, such as assignments, branch updates,
or merge actions.

### Supersede

To supersede is to replace an older version, verdict, or work item with a newer
one and mark the older one as no longer authoritative. For example, a moved PR
head can supersede a SHA-bound review verdict and require fresh review over the
new state.
