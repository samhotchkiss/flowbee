# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Engine milestones (M0–M12)

- **M0 — Bootstrap.** Project scaffolding, configuration, and the baseline run loop.
- **M1 — Ingest.** Intake of specs and issues into the engine's work queue.
- **M2 — Job model.** Core job records, lifecycle states, and persistence.
- **M3 — Scheduling.** Job selection, dependency ordering, and dispatch.
- **M4 — Workers.** Worker pool that claims and executes scheduled jobs.
- **M5 — Repo binding.** Associating jobs with their target repositories.
- **M6 — Spec drain.** Draining spec jobs through the project pipeline.
- **M7 — Materialization.** Rendering materialized issues from spec content.
- **M8 — Build jobs.** Seeding build jobs once a signed-off issue materializes.
- **M9 — Branch publish.** Publishing build branches to GitHub.
- **M10 — PR wiring.** Opening pull requests from completed build results.
- **M11 — Status reporting.** Surfacing job and pipeline status to operators.
- **M12 — Observability.** Logging, metrics, and run introspection.

### Flow-pass milestones (F1–F14)

- **F1 — Spec front door.** `POST /v1/specs` planner intake endpoint.
- **F2 — Spec retention.** Retaining `spec.md` bytes alongside the job.
- **F3 — Repo resolution.** Binding spec jobs to their owning repo.
- **F4 — Project-OUT.** Draining bound spec jobs out of the project stage.
- **F5 — Issue render.** Rendering issues from spec content instead of stubs.
- **F6 — Sign-off.** Gating materialization on issue sign-off.
- **F7 — Build seed.** Seeding a build job from a signed-off issue.
- **F8 — Build run.** Executing the build job against its repo.
- **F9 — Branch push.** Pushing the build branch to GitHub.
- **F10 — PR open.** Opening the pull request for a build result.
- **F11 — Result wiring.** Linking build results back to their source issue.
- **F12 — Feedback loop.** Propagating PR and build status upstream.
- **F13 — Retry & recovery.** Re-driving failed jobs through the flow.
- **F14 — End-to-end pass.** Full spec-to-PR flow validated end to end.
