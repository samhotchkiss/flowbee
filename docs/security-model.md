# Flowbee Security Model

This page describes what Flowbee defends against and the concrete guardrails
that defend it. Flowbee orchestrates fleets of AI coding agents that produce and
review real changes to your repository. Those agents — and the content flowing
through them — are **not fully trusted**: an agent can be buggy, prompt-injected,
or simply wrong. Flowbee's job is to let untrusted work flow through the pipeline
while guaranteeing that only independently reviewed, unmodified code reaches
`main`. This document is the authoritative reference for operators, contributors,
and security reviewers on each named guard, the threat it blocks, and how it
fails safe.

## Threat model

### Trust boundaries

Flowbee is the **control plane** and the system of record for the process. It is
deterministic, has no LLM, and is the only component that talks to GitHub. The
intelligence — and therefore the untrustworthiness — lives at the edges.

- **Workers are untrusted execution units.** A worker is a thin, self-identifying
  pull-loop that wraps some coding agent (Codex, Claude, a local model, …). It
  dials *out* to Flowbee, leases a job, runs an agent against a git working copy,
  and reports back. A worker may be compromised, prompt-injected through the
  content it processes, or just buggy. Its self-reported results are **hints, not
  facts**.

- **Content/source is untrusted.** The diffs, specs, and patches moving through
  the pipeline originate from agents acting on instructions that may themselves
  be adversarial (e.g., a prompt-injection payload embedded in an issue or a
  source file). Content can attempt to escalate privilege — for example by
  editing CI config, lockfiles, or Flowbee's own orchestrator source.

### Integrity properties Flowbee must preserve

1. **A verdict reflects the exact code that was reviewed.** An approval is bound
   to specific content; it cannot be replayed onto, or transplanted to, different
   code than what was evaluated.
2. **A worker cannot approve its own work.** The actor (and correlated actors)
   that produced a change must not be the one that signs off on it or merges it.
3. **A compromised or buggy worker cannot poison shared state.** Verdicts and
   merges derive from facts Flowbee reconciles directly from GitHub, never from a
   worker's say-so.

### Out of scope

The threat model deliberately does **not** cover:

- **Host / infrastructure compromise.** If the machine running the control plane
  or its SQLite store is compromised, or the GitHub token is stolen, the guards
  below do not apply. Protect the host and credentials by other means.
- **Trusted-operator misuse.** An operator who deliberately disables a guard,
  merges by hand on GitHub, or alters the store is outside the model. Flowbee
  defends against untrusted *work*, not against its own administrator.
- **The agents' internal behavior.** Flowbee treats agents as black boxes and
  verifies their *output*; it does not attempt to sandbox or audit what happens
  inside a given agent process beyond the isolation described below.

## Guards

Each guard below states *what it is*, *the threat it mitigates*, and its
*failure-safe behavior*. Every guard is designed to **fail closed**: when a check
cannot be satisfied, the safe outcome (deny, quarantine, or hand to a human) is
taken rather than the permissive one.

### Untrusted workers

**What it is.** Workers are treated as untrusted, least-privilege execution
units. A worker holds no GitHub credentials and never talks to GitHub — only the
control plane does. It operates on a local git working copy scoped to its leased
job, fenced by an epoch-bound, exactly-once lease so a stale or zombie worker
cannot clobber a reassigned job. Crucially, Flowbee **never trusts a worker's
self-reported result as a fact**: a worker reporting "approved" or "succeeded"
produces only a claim. Authoritative verdicts are derived from facts Flowbee
reconciles directly from GitHub (the PR exists, CI is green, the SHAs are
consistent).

**Threat mitigated.** A compromised, prompt-injected, or buggy worker that lies
about its outcome — claiming an unreviewed or failing change is approved — or a
zombie worker that resumes after reassignment and overwrites current state.

**Fail-safe behavior.** A claim that is not backed by green, consistent
reconciled facts does not advance: it bounces back toward build rather than
forward toward merge. Absence of corroborating facts is treated as failure, not
success. The lease epoch fences out stale workers, so late writes from a
superseded worker are rejected.

### Content-integrity denylist (`flowbee_source`)

**What it is.** Before a change can merge autonomously, its diff is screened
against a **content-integrity denylist**. The denylist enumerates categories of
paths whose modification is too dangerous to approve without a human, including:

- CI/workflow configuration (a diff could weaken the very gate that judges it),
- dependency lockfiles and lifecycle/install scripts (arbitrary code at install
  time),
- container and devcontainer definitions (supply-chain substitution),
- secrets and credential material, and
- **Flowbee's own orchestrator source** — the `flowbee_source` matcher. This
  covers Flowbee's internal packages, command, tooling, flow definitions, and
  the denylist rules themselves. An agent that tries to rewrite the rules it is
  judged by, or to escalate privilege by editing the control plane, is caught
  here.

**Threat mitigated.** A prompt-injected or malicious diff that attempts to
disable a guard, exfiltrate secrets, plant install-time code, or modify the
orchestrator to grant itself authority — i.e., attacks that cross the
content-integrity boundary by changing privileged files rather than ordinary
application code.

**Fail-safe behavior.** A diff that touches any denylisted path is marked not
clear. The change is **not** eligible for autonomous self-merge; it is forced to
the human handoff path (quarantined from `main`) regardless of other approvals.
The matched paths are recorded for audit. Because `flowbee_source` covers the
denylist's own definition, the screen cannot be silently removed by the content
it screens.

### Anti-affinity

**What it is.** Anti-affinity is enforced at lease time: the system refuses to
hand a review or merge role to an actor that is correlated with the actor who did
the upstream work. Concretely, the worker that *built* a change cannot lease the
*review* of that change (same identity is excluded), and a worker whose **model
family** matches the builder's is likewise excluded from reviewing it. The merge
role similarly excludes the reviewer's identity.

**Threat mitigated.** Self-review (an actor approving its own work) and
**correlated review** — a same-family or same-lens reviewer that shares the
builder's blind spots and would rubber-stamp the same mistake or the same
injected payload. Independence between roles is the precondition that makes the
other guards meaningful: SHA-binding and self-merge gating only enforce
"independently reviewed" if the reviewer is genuinely independent.

**Fail-safe behavior.** Anti-affinity is expressed as exclusion conditions on the
atomic claim. If granting the lease would violate independence, the claim matches
no eligible row and **no lease is issued** — the job simply waits in its pending
state until a genuinely independent worker is available. The default is to
withhold the role, never to relax the constraint to make progress.

### SHA-bound verdicts

**What it is.** A verdict (a review/approval result) is cryptographically bound
to the exact content it was rendered against — specifically the head and base
content `SHA`s — via a deterministic, tamper-evident integrity hash computed over
the verdict's value, disposition, the bound SHAs, and its provenance. The verdict
carries the SHAs it was minted for.

**Threat mitigated.** Replay and transplant attacks: taking an approval that was
earned by reviewed code and applying it to *different* code — for instance by
pushing new commits after approval, or moving the base — so that unreviewed
changes inherit a stale sign-off.

**Fail-safe behavior.** At consumption time (when deciding whether a change may
merge), Flowbee re-verifies the verdict against the **currently reconciled** head
and base SHAs and recomputes the integrity hash. On any mismatch — a moved SHA or
a tampered field — verification fails: the verdict is treated as invalid, the
change is not eligible to merge, and the job is superseded and re-armed for fresh
review over the new SHA (with the lease epoch bumped to fence any worker still
acting on the old state). A moved SHA provably invalidates the sign-off.

### Self-merge gating

**What it is.** Autonomous merge to `main` ("self-merge") is allowed only when a
single canonical eligibility predicate holds. That predicate composes the other
guards: it requires that policy explicitly permits self-merge, that the
content-integrity denylist is clear, that the change's blast radius is consistent
with what was declared, that static checks pass, and that the SHA-bound verdict
still verifies against the reconciled SHAs. The verdict itself is always minted
from green reconciled facts; self-merge is a *further* promotion gated on the
full predicate.

**Threat mitigated.** An actor merging its own (or correlated) change, or a
change reaching `main` without independent review, clean content, or an intact
binding — i.e., any breakdown of separation of duties between build, review, and
merge.

**Fail-safe behavior.** The predicate is deny-by-default. If policy is off (the
default), or content is not clear, or the blast radius is inconsistent, or static
checks fail, or the SHA binding no longer verifies, self-merge is denied and the
change is routed to **human handoff** instead. The change is still recorded as
reviewed, but it does not merge autonomously. Absence of proof (e.g., missing
content result) is treated as denial. Workers neither see nor control this
predicate; it is evaluated deterministically in the control plane.

## How the guards compose

The guards are layered so that defeating one is not enough to ship unreviewed or
tampered code. Together, **anti-affinity + SHA-bound verdicts + self-merge gating
enforce the core invariant: the code that ships is the code that was
independently reviewed.**

| Guard | Boundary it holds | Without it, an attacker could… |
| --- | --- | --- |
| Untrusted workers | Worker → control plane | Have a lying worker assert its own approval |
| `flowbee_source` denylist | Content → privileged files | Edit CI, secrets, or the orchestrator to escalate |
| Anti-affinity | Build role ↔ review/merge role | Review or merge its own (or a correlated) change |
| SHA-bound verdicts | Approval ↔ exact content | Replay a stale approval onto new, unreviewed code |
| Self-merge gating | Reviewed change → `main` | Merge despite a failed check or broken binding |

The composition is concrete: anti-affinity guarantees the reviewer is a
*different, uncorrelated* actor; the SHA binding guarantees that reviewer
evaluated *this exact content*; the denylist guarantees the content did not touch
privileged paths; and self-merge gating refuses to merge unless **all** of those
hold simultaneously. Because verdicts derive from reconciled facts rather than
worker claims, a compromised worker cannot shortcut any of these checks.

## Operator notes / fail-safe summary

Every guard **fails closed** — the default outcome is to deny, quarantine, or
hand off to a human, never to permit:

- A worker claim with no corroborating reconciled facts does not advance.
- A diff touching a denylisted path (including `flowbee_source`) cannot
  self-merge.
- A role that would violate anti-affinity is simply not leased.
- A verdict that no longer verifies against the current SHAs is discarded and the
  work is re-reviewed.
- Self-merge requires the full predicate; any unmet condition routes to human
  handoff.

**Grounding the anti-affinity axis (`model_family`).** Anti-affinity compares the
builder's and reviewer's `model_family`. That value is supplied by the worker, so on
an authenticated deployment you should **bind it to the enrolled identity** rather than
trust the worker's word: write enrolled entries as `identity:family` (e.g.
`enrolled_identities: ["reviewer-bob:claude-opus", "builder-ann:codex"]`). The control
plane then clamps each worker's self-asserted `model_family` to the operator-declared
value, so an enrolled worker cannot lie about its family to review a same-family (or its
own model's) build. A bare `identity` entry leaves `model_family` worker-asserted — fine
on a fully trusted single-operator fleet, but bind families whenever distinct workers
share one enrolled secret. (The *identity* axis is always credential-bound: a worker can
never review its own build regardless.)

Operators can observe enforcement through Flowbee's records: minted verdicts and
their dispositions, recorded denylist hits (for audit of why a change was held),
lease activity (including roles that were withheld for anti-affinity), and
supersession/re-arm events when a SHA moves. Because the control plane is
deterministic and replayable from its event log, the reason any change did or did
not merge can be reconstructed after the fact. Disabling a guard — for example,
turning self-merge policy off — is an operator action and falls outside the
threat model; the safe default is for autonomous merge to be **off** until
explicitly enabled.

See also: [operating.md](operating.md)
