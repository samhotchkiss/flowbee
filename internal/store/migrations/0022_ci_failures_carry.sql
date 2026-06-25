-- 0022: carry the failing CI check NAMES forward to the rebuild (compounding memory, §F read).
-- When a build's CI is definitively red and the job bounces back to build, the names of the
-- checks that failed (e.g. "Architecture and guardrail lints", "golangci-lint") are now
-- persisted on the job so the next build's lease context can surface them — the agent re-runs
-- the named gate locally and fixes the real violation instead of rebuilding blind and
-- re-submitting a change that fails the same check. Mirrors last_review_notes (0020); a
-- projection field folded from the ci-fail bounce event (projection == Fold(events)).
ALTER TABLE jobs ADD COLUMN last_ci_failures TEXT NOT NULL DEFAULT '';
