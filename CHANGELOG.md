# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Engine milestones (M0–M12)

- **M0 — Bootstrap:** Project scaffolding, configuration, and the baseline runtime skeleton.
- **M1 — Task model:** Core task representation, lifecycle states, and persistence.
- **M2 — Dispatch:** Task dispatch loop that moves work between lifecycle states.
- **M3 — Roles:** Role definitions (planner, builder, reviewer, merger) and role-based routing.
- **M4 — Workspaces:** Isolated per-task workspaces seeded from a base SHA.
- **M5 — Ingest:** Planner front-door (`POST /v1/specs`) and retention of spec bytes.
- **M6 — Spec binding:** Bind a spec job to its originating repo so the project queue drains it.
- **M7 — Build jobs:** Seed a build job when a signed-off issue materializes.
- **M8 — Build publish:** Publish the build branch to GitHub and open the pull request.
- **M9 — Review:** Reviewer pass over the build diff with structured findings.
- **M10 — Merge:** Autonomous merge — enqueue the GitHub merge on dispatch to `merging`.
- **M11 — Observability:** Structured logging, status surfaces, and run introspection.
- **M12 — Hardening:** Error handling, retries, and end-to-end stabilization.

### Flow-pass milestones (F1–F14)

- **F1–F14:** Iterative flow-pass refinements wiring the planner → builder → reviewer →
  merger pipeline end to end, tightening state transitions, queue draining, and the
  GitHub integration across the full task lifecycle.

[Unreleased]: https://keepachangelog.com/en/1.1.0/
