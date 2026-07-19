---
pagetitle: "Flowbee v2 — High-Level Product & Operating Model"
lang: en-US
---

<section class="cover">

<div class="eyebrow">ALIGNMENT REVIEW · JULY 2026</div>

# Flowbee v2

## High-Level Product & Operating Model

Flowbee becomes the durable operating system for a small, AI-powered engineering organization: one calm place for the human, clear roles for every agent, and a pipeline that keeps moving without constant supervision.

<div class="cover-note">

**Purpose of this brief**
Confirm that we agree on *what we are building* before the technical design is implemented.

This document intentionally leaves out APIs, schemas, commands, and other implementation mechanics.

</div>

<div class="version">REVIEW DRAFT · V0.2</div>

</section>

<div class="page-break"></div>

# The north star

The finished system should feel less like supervising a collection of terminal sessions and more like running an engineering organization from one dependable home screen.

A human discusses goals, priorities, plans, and product decisions with a high-capability **Interactor**. Once work is sufficiently defined—and any required approval has been given—it moves automatically into delivery. The human should never need to say, “Now push this into Flowbee.”

Behind that focused conversation:

- a project **Orchestrator** turns product intent into well-shaped epics;
- **Flowbee** owns the durable logistics of delivering those epics;
- builders and reviewers do focused work in isolated sessions;
- the dashboard shows what is happening, what is blocked, and what genuinely needs a human.

The system keeps moving through restarts, usage interruptions, agent failures, and handoff gaps. Problems become visible states with owners and recovery paths—not silent stalls discovered by asking whether anything is building.

<div class="callout">

**The promise:** define work once, approve only what requires judgment, and trust the system to carry it safely to completion—or clearly surface why it cannot.

</div>

<div class="page-break"></div>

# The operating model

<div class="topology">

<div class="node human">HUMAN</div>
<div class="arrow">↓</div>
<div class="node dashboard">ONE FLOWBEE DASHBOARD</div>
<div class="arrow">↓</div>
<div class="pair"><span>PROJECT INTERACTOR</span><span>one per project</span></div>
<div class="arrow">↕</div>
<div class="pair"><span>PROJECT ORCHESTRATOR</span><span>one per project</span></div>
<div class="arrow">↓</div>
<div class="node flowbee">ONE LOGICAL FLOWBEE</div>
<div class="fork"><span>↙ BUILDERS</span><span>REVIEWERS ↘</span></div>

</div>

There is one human-facing dashboard across all projects. Each project has its own Interactor–Orchestrator pair, preserving project context and responsibility. All projects share one logical Flowbee authority and a common fleet of builders and reviewers.

“One Flowbee” means one authoritative view of work, scheduling, and state. It does **not** mean one fragile process or one physical machine.

<div class="profile">

**Default profile**
Interactor: Claude/Fable · Orchestrator: Codex · Flowbee operations: Grok · Builders: Codex · Reviewers: Grok

These are configurable assignments. Role boundaries and required capabilities matter more than vendor names.

</div>

<div class="page-break"></div>

# Who is responsible for what

## Human

Sets direction, makes product and design decisions, approves what truly requires human judgment, and resolves escalations that agents cannot safely answer.

## Interactor

The human’s project conversation. It maintains the high-level thread, helps shape plans, recognizes when ordinary requests are ready to become work, and brings meaningful questions back to the human.

## Orchestrator

Owns project logistics. It turns intent into epics, groups related issue references when useful, answers product questions when it can, and escalates genuine ambiguity to the Interactor.

## Flowbee

Owns delivery logistics for epics: admission, assignment, monitoring, review routing, rework, merge conflicts, completion, cleanup, and operational escalation. Its durable core is deterministic; a bounded operational agent handles routine session-level intervention.

## Builders and reviewers

Builders implement an epic. Independent reviewers judge the result. A rejected build returns to its original builder context whenever possible.

## Tmux Driver

Provides transport and actuation for agent sessions. It can observe and send messages, but it is never the memory or source of truth for the workflow.

<div class="page-break"></div>

# Flowbee owns epics—not loose work

Flowbee’s unit of responsibility is the **epic**. It does not independently adopt one-off issues or PRs.

An Orchestrator may create:

- an epic containing a single issue-sized objective; or
- an epic that groups several related issue references into one coherent delivery.

Issues are planning inputs. Branches and pull requests are delivery outputs. By default, one epic produces one isolated branch, one reviewable change, and one pull request.

This boundary matters because it gives every piece of active work a clear owner, scope, lifecycle, and definition of done. A PR can never become an orphaned object that Flowbee notices without understanding the epic it belongs to.

## Project does not mean repository

A project is a durable product context and may include multiple repositories. In the initial model, each epic has one delivery repository and one PR. Larger cross-repository initiatives are represented as several dependency-linked epics rather than one atomic, multi-repository epic.

<div class="callout">

**Simple rule:** Orchestrators shape epics; Flowbee delivers epics; branches and PRs exist in service of those epics.

</div>

<div class="page-break"></div>

# How work moves

1. **A need appears.** The human discusses a goal with the project Interactor, or the project already contains a clearly actionable request.

2. **Intent becomes explicit.** The Interactor recognizes when the request is sufficiently defined. If a plan, design, or other human decision is required, it creates that exact approval request and waits.

3. **Promotion is automatic.** Once the request is ready—and required approvals are satisfied—it moves to the paired Orchestrator without a second human “go” command.

4. **The Orchestrator prepares an epic.** It defines the delivery boundary, acceptance intent, relevant issue references, project, and target repository.

5. **Flowbee admits and delivers the epic.** It reserves safe capacity, assigns an isolated builder context, observes progress, routes the completed build through independent review, handles rework, and merges when the gates are satisfied.

6. **The dashboard remains the shared view.** Normal progress is quiet. Decisions, risks, and genuine exceptions rise to the top. Completion is recorded and the temporary delivery environment is cleaned up.

<div class="callout soft">

Approval is attached to the exact plan or design version reviewed. If that artifact changes materially, the system asks again rather than stretching an old approval.

</div>

<div class="page-break"></div>

# Durable and self-healing by design

The workflow must survive the loss of any individual conversation or terminal session. Important intent is recorded before an agent is asked to act.

That changes the failure model:

- If a dispatcher is interrupted after a build finishes, the need for review still exists.
- If a reviewer dies, the review becomes eligible for safe reassignment.
- If a builder pauses because of account usage, its epic remains owned and visible.
- If a process restarts, it reconstructs work from durable state rather than remembered chat context.
- If an expected transition does not happen, reconciliation detects the mismatch, repairs it when safe, and raises an alert.

The production stall that motivated this work—CI green, no reviewer verdict, no active reviewer, and no alert—becomes a named, visible state:

<div class="status">BUILT · AWAITING REVIEW DISPATCH</div>

It may exist briefly during normal operation. It may not remain invisible or indefinite.

Every actor receives a versioned role and recovery contract automatically. Recovery must not depend on a human remembering which instruction to paste into which session.

<div class="page-break"></div>

# Throughput without collisions

Flowbee should be able to land multiple simultaneous epics on the same host—and, where supported, through the same agent account—without allowing them to overwrite or confuse one another.

Each active epic has an isolated delivery context:

- its own branch;
- its own worktree or equivalent filesystem boundary;
- its own agent session;
- its own durable identity and scope;
- explicit ownership of the artifacts it may change.

Concurrency is governed by real capacity, not by the assumption that one host equals one job. A capable host may run several builders at once while reviewer capacity remains independently available.

When a build enters CI or review, its builder context stays parked for fast, coherent rework. The expensive active-compute slot can be released, but the branch, worktree, session identity, and ownership remain protected. If review rejects the change, Flowbee resumes the same builder context instead of starting from scratch.

Isolation prevents simultaneous epics from “squashing” one another. Merge conflicts are explicit delivery events, handled with full knowledge of both the epic and its current artifact—not accidental cross-session damage.

<div class="page-break"></div>

# Foundation: earn trust first

Before the dashboard becomes the human’s primary workspace or the fleet expands across projects, Flowbee must be dependable at the two places where silent failure is most dangerous.

## 1. Pipeline continuity

Build-to-review intent is durable. Missing handoffs are detected and repaired. Rejections return to the correct builder. Merge, conflict resolution, and cleanup remain owned until the epic is truly complete. Every stalled transition is visible and alertable.

## 2. Trustworthy capacity

Codex and Grok usage are measured from live, identity-bound observations. Account-wide quota is separated from the health of an individual seat. Shared accounts seen from multiple hosts are counted once. Stale, cached, unknown, or display-only data can be shown, but it cannot be used to route new work.

The system fails closed when it cannot prove that capacity is safe. “Reviewer pool available” must mean something operationally reliable, not merely that a dashboard last saw a reassuring number.

## Foundation outcome

Flowbee can be interrupted, restarted, or temporarily lose an agent and still explain what it owns, what should happen next, and how progress will resume.

<div class="page-break"></div>

# Phase 1: the single-project operating system

Phase 1 turns the durable pipeline into a dashboard-first way of operating one project end to end.

## What changes for the human

- The Flowbee dashboard becomes the normal home screen.
- The human can converse with the project Interactor there.
- Plans, designs, and questions arrive as clear, typed decisions.
- Approvals apply to an exact version and leave an audit trail.
- Ordinary ready work advances automatically; no manual plumbing prompt is required.
- “Needs You” contains only items that genuinely require human judgment or authority.

## What changes for the agents

- Interactor, Orchestrator, Flowbee, builders, and reviewers receive explicit role boundaries.
- Escalations travel up the chain with context and ownership.
- Answers travel back down and automatically unblock the waiting work.
- Operational details stay below the human-facing conversation unless they become relevant.

## Phase 1 outcome

For one project, a human can set direction, review decisions, and understand delivery from the dashboard while Flowbee reliably moves epics through build, review, rework, and merge.

The terminal remains available for deep debugging, but it is no longer the product’s primary interface.

<div class="page-break"></div>

# Phase 2: the multi-project engineering portfolio

Phase 2 extends the Phase 1 model across several products and repositories without collapsing their contexts into one conversation.

## One dashboard, many project pairs

Each project has one Interactor paired one-to-one with one Orchestrator. That pair owns the project’s language, goals, decisions, and backlog. The global dashboard gives the human one portfolio view and a clean way to enter any project conversation.

## One shared Flowbee fleet

All project Orchestrators submit epics to one logical Flowbee authority. Flowbee allocates a shared pool of builders and reviewers fairly, with project-aware limits and priorities. Spare capacity can help another project, but one noisy or broken project cannot silently starve the rest.

## Project and repository boundaries remain explicit

A project may span repositories. Every epic still identifies one project and one delivery repository. Cross-project and cross-repository dependencies are visible relationships with separate ownership, not hidden coupling.

## Phase 2 outcome

The human can run a portfolio from one home screen: seeing which projects are healthy, where capacity is going, which decisions need attention, and whether any project is stalled—without monitoring a wall of terminals.

<div class="page-break"></div>

# Foundation, Phase 1, and Phase 2

| | Foundation | Phase 1 | Phase 2 |
|---|---|---|---|
| Primary goal | Make delivery durable and capacity trustworthy | Make the dashboard the home for one project | Run many projects through one portfolio |
| Human experience | Stalls and unsafe capacity are visible | Conversation, decisions, and delivery in one place | Global overview plus project-specific conversations |
| Work movement | Handoffs recover automatically | Ready intent promotes automatically | Epics share fleet capacity fairly |
| Scope | Core epic pipeline | One project context | Many project contexts and repositories |
| Agent model | Explicit roles and recovery contracts | One Interactor–Orchestrator pair | One pair per project |
| Flowbee | One durable delivery authority | Operates the project pipeline | Operates the shared portfolio fleet |
| Success looks like | No silent stall survives | Human rarely needs the terminal | Projects scale without context or capacity collisions |

<div class="callout">

These are cumulative layers, not alternative products. Phase 1 depends on the Foundation. Phase 2 preserves the Phase 1 experience inside each project while adding portfolio coordination above it.

</div>

<div class="page-break"></div>

# A day in the finished system

In the morning, the human opens Flowbee—not a terminal. The portfolio says that two projects are building, one is in review, and one design decision needs attention.

The human enters that project’s Interactor conversation, reviews the proposed design, and approves the exact version shown. That is the only manual step. The waiting intent resumes automatically, the Orchestrator shapes the epic, and Flowbee assigns it when safe capacity is available.

During the day, several epics build simultaneously on the same host in isolated contexts. One finishes and moves into review. Another is rejected and returns to its original builder with the review verdict attached. A third pauses because its account capacity is no longer safe; the dashboard explains why and routes other work elsewhere.

A dispatcher session is interrupted by a usage dialog just after CI turns green. Nothing is lost. The durable review intent remains, reconciliation notices that no reviewer is active, the review is dispatched exactly once, and the event appears in the audit trail.

The human sees none of the terminal traffic. They see a calm summary, one genuine decision, and later a completion notice. If they choose to investigate, every state and handoff is explainable.

<div class="page-break"></div>

# Non-negotiable product principles

1. **The dashboard is the home; the terminal is a diagnostic tool.**
2. **Flowbee owns epics only.** Loose issues and PRs do not become shadow work.
3. **Ready work advances automatically.** Human approval is a judgment gate, not a plumbing command.
4. **Durable state outranks session memory.** Restarting an agent cannot erase an obligation.
5. **No silent stalls.** Missing progress becomes a visible state, an owned recovery action, and an alert.
6. **Concurrency requires isolation.** Every epic has protected branch, workspace, session, and scope.
7. **Review is independent.** Model diversity and explicit reviewer ownership protect quality.
8. **Capacity must be live and attributable.** Unknown or stale headroom never authorizes new work.
9. **Project context stays project-scoped.** Portfolio sharing must not blur decisions or responsibility.
10. **All actors know the operating contract.** Roles, escalation paths, and recovery behavior are distributed automatically and versioned.

## Deliberately not in this version

A generic issue bot; opportunistic adoption of unrelated PRs; one atomic epic spanning several repositories; an LLM making authoritative state transitions; a terminal-first control surface; or hidden workflow state that exists only inside a chat or tmux session.

<div class="page-break"></div>

# Alignment review

Mark anything that needs discussion.

<div class="checks">

□ The dashboard should become my normal home screen.

□ Each project should have one Interactor paired with one Orchestrator.

□ All projects should share one logical Flowbee authority and fleet.

□ Flowbee should own epics—not one-off issues or PRs.

□ One epic should normally produce one branch and one PR.

□ A project may contain multiple repositories.

□ Cross-repository initiatives should begin as linked epics, not one atomic epic.

□ Ready requests should move into delivery automatically after required approvals.

□ The system should keep builder context parked through review for coherent rework.

□ Multiple epics may run simultaneously on one host when isolation and live capacity permit.

□ Unknown or stale Codex/Grok usage should block new routing, not guess.

□ Every actor should automatically receive its role, escalation, and recovery contract.

□ The Foundation → Phase 1 → Phase 2 sequence reflects the intended build order.

</div>

<div class="page-break"></div>

## Notes

<div class="notes"></div>
