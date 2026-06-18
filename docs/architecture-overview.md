# Architecture Overview

Flowbee orchestrates a fleet of AI coding agents; this page is the one-page map of
how the system is shaped — read it first, before the deeper design docs.

## Control plane vs. workers

Flowbee is split into two kinds of component.

The **control plane** is the brain. It holds the *desired* state of the world — the
job graph, who owns what, what should happen next — makes every scheduling and
ownership decision, and hands out work one piece at a time. It is deterministic and
carries no LLM: its behavior is replayable and its history folds from an event log.

**Workers** are thin, self-identifying pull-loops. They dial *out* to the control
plane, lease a job, execute it with whatever agent they wrap (Codex, Claude, a local
model), and report the *observed* result back. Workers never decide what to do next
and never talk to external systems like GitHub on their own.

The split also fixes who is the source of truth for what: the control plane owns
**desired state**, and workers (plus the outside world) supply **observed state**.
Communication always originates at the worker — it pulls work and pushes results;
the control plane never reaches into a worker.

## The two domains

Data flows in two directions, and it helps to name them.

| Domain          | Direction        | Carries                                   |
| --------------- | ---------------- | ----------------------------------------- |
| **reconcile-in**  | world → control plane | observed/actual state flowing inward  |
| **project-out**   | control plane → world | desired state / decisions flowing outward |

**reconcile-in** is the inbound loop: observed facts — a worker's result, a CI
status, the real state of an external system — flow *in* so the control plane can
reconcile actual state against desired state and decide what changed.

**project-out** is the outbound loop: the control plane's decisions and desired
state are *projected out* to workers (and to external systems) so they can act on
them.

The control plane sits at the hinge of both — it consumes reconcile-in and emits
project-out. Workers participate at the edges of both: they produce the observations
that feed reconcile-in and consume the assignments that come from project-out.

## Lease / epoch fencing

Because work is distributed, the control plane must guarantee that a job belongs to
exactly one worker at a time. It grants ownership of a resource as a **lease**, and
each lease carries an **epoch** — a monotonically increasing fencing token.

The failure this prevents is the zombie actor: a worker that is slow, paused, or
network-partitioned may still believe it holds a lease long after that lease has
been revoked and re-granted to someone else. Without fencing, the stale worker could
wake up and clobber work the new owner has already done.

The mechanism is simple: every write carries the epoch the writer thinks is current.
A write whose epoch is stale (lower than the resource's current epoch) is rejected.
Re-granting a lease always bumps the epoch, so a new owner holds a strictly higher
epoch — and the old owner is automatically fenced out the moment it tries to write.

## The gate

The **gate** is the single chokepoint every relevant write must pass through before
it takes effect. Centralizing admission control in one place means the fencing and
invariant checks cannot be bypassed by any code path: there is exactly one door, and
everything goes through it.

The gate is where the epoch check from above is enforced — it rejects writes carrying
a stale epoch — and it is also where other admission invariants live (for example,
content-integrity checks that keep a tampered or prompt-injected diff out of `main`).
A write is admitted only if it is both *fenced-valid* (current epoch) and
*invariant-valid*.

## How it fits together

The whole system is one loop, with the gate guarding every write:

```
workers observe ──reconcile-in──▶ control plane decides ──project-out──▶ workers act
        ▲                                                                     │
        └─────────────────────────────────────────────────────────────────────┘
                         (the gate fences stale actors at every write)
```

Workers observe and report; observations reconcile *in*; the control plane reconciles
desired against actual and decides; decisions project *out* to workers; workers act —
and at every write, the gate enforces the lease/epoch fence so no stale actor can
touch a resource it no longer owns.

See also: [operating.md](operating.md)
