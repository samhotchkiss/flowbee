# Migration number ladder

This file is the single source of truth for which migration NUMBERS are taken.
It exists because the number space is a shared resource with no other arbiter:
the runtime (`internal/store/migrate.go`) keys applied migrations on their full
FILENAME, so two branches that independently pick `0023_*.sql` both apply
cleanly — and the fleet has already done exactly that, twice (see History
below). A silently-duplicated number is invisible at runtime but corrupts the
ordering assumption every future migration relies on.

## How to take a number

Do NOT hand-pick a number. Run:

```
flowbee migration reserve <slug>
```

from the repo root. It takes an exclusive lock on this file, appends the next
free number, and prints the reserved filename (e.g. `0031_my_thing.sql`). Then
create exactly that file under `internal/store/migrations/`.

The file lock only serializes reservations made on the SAME host (flock is
same-host advisory locking). Across separate machines or worktrees, the real
backstops are the git merge — two branches that each appended a different slug
at the same number produce a merge conflict in this file — and `laddercheck`,
which fails CI if any migration's number is unreserved or duplicated. Reserving
before writing keeps those conflicts rare and legible instead of silent.

`tools/laddercheck` (run in CI alongside archcheck/providerlint, and via
`make laddercheck`) fails the build if any `migrations/*.sql` carries a number
that is absent from this ladder or duplicates one beyond a sanctioned double.

## Reserved numbers

Machine-managed — the allocator appends here; do not reorder or hand-edit.
Reservations may run ahead of the actual `.sql` file (a number claimed before
its migration is written); that is expected and not a violation.

<!-- ladder:reserved:begin -->
```text
0001_init
0002_m1_lease_thread
0003_m2_scheduler
0004_m3_flow
0005_m4_antiaffinity
0006_m5_worker_harness
0007_m6_reconcile
0008_m7_project_out
0009_m8_liveness
0010_m9_content
0011_m10_cost
0012_m11_epoch
0013_f1_task_context
0014_f4_epic_review
0015_f6_capacity
0016_f7_board_lifecycle
0017_f8_merge_conflicts
0018_f9_multirepo
0019_f10_test_ci
0020_review_notes_carry
0021_priority_lower_is_urgent
0022_ci_failures_carry
0023_adopted_pr_diff_empty
0023_self_unblock
0024_advisor
0024_mergeable_state_fact
0025_goal_sessions
0026_epics
0027_epic_attention
0028_epic_capacity
0029_epic_drift_ci
0030_missions
0031_seat_concurrency
0032_v2_control_plane
0033_phase1_decisions
0034_v2_capacity_observations
0035_capacity_collector_backoff
0036_phase1_work_intents
0037_phase1_work_intent_transport
0038_phase1_conversations
0039_phase1_epic_contracts
0040_v2_builder_launch
0041_phase1_human_login
0042_phase1_decision_delivery
0043_phase2_project_ownership
0044_phase1_conversation_delivery
0045_phase2_project_attribution
0046_phase2_project_breakers
0047_phase2_project_fair_dispatch
0048_driver_control_principal
0049_driver_reviewer_seat
0050_driver_tmux_server_domain
0051_driver_external_watch_authority
0052_project_actor_lifecycle_outbox
0053_interactor_control_alert_projection
0054_deadman_ingress_storage
0055_external_watchdog_heartbeat
0056_phase2_outbox_project_attribution
0057_phase2_attention_project_isolation
0058_phase2_builder_fair_dispatch
0059_phase2_repo_admission_routing
0060_control_plane_incarnations
0061_phase2_artifact_cost_attribution
0062_phase2_epic_worker_lifecycle
0063_phase2_epic_repository_sets
0064_bootstrap_action_ledger
0065_phase2_actor_q3_materials
0066_phase2_adopted_interactor_recovery
0067_epic_worker_pre_effect_cleanup
```
<!-- ladder:reserved:end -->

The `0027`–`0030` reservations are the epic-lane plan's forward allocations
(§9): `0027_epic_attention` (Phase 5 — supervisors + attention_items),
`0028_epic_capacity` (Phase 6 — account_windows + epic account/context columns),
`0029_epic_drift_ci` (Phase 8 — review notes + drift/CI fact columns),
`0030_missions` (Phase 9). Their `.sql` files do not exist yet — no migration
lands in Phase 4 — but the numbers are claimed so the parallel Phase 5/6/8/9
builders cannot re-collide the way 0023 and 0024 did.

## History (grandfathered doubles + a superseded number)

These predate the ladder and are seeded as-is; the checker sanctions them:

- **Double `0023`** — `0023_adopted_pr_diff_empty` and `0023_self_unblock` were
  authored on concurrent branches and both applied (keyed by filename). Both are
  live on main.
- **Double `0024`** — likewise `0024_advisor` and `0024_mergeable_state_fact`.
- **Superseded `0023_preemptive_usage_budget`** — the
  `feat/windowed-token-budget-capacity` branch also picked `0023`, for a
  guessed-budget capacity table. It was **never applied** and is **consciously
  superseded** by the acctprobe-fed `0028_epic_capacity` (plan §4.1): the "Codex
  exposes no live %" premise it rested on is obsolete. Its `.sql` is not in this
  tree; the number is not re-reservable (0023 is spent) and is recorded here only
  so a future reader knows why the capacity work jumps to 0028.

The ladder is the fix that stops the next collision: numbers 0027+ are now
allocated, never guessed.
