-- F1: the lease carries the job's task/context. A job now persists the human
-- intent (task/spec text + acceptance criteria) so the lease grant can ship a
-- self-contained context block to an untrusted worker — the worker reads it from
-- the worktree (.flowbee/task.md) and env, and can never choose its own identity
-- or task (both are resolved by Flowbee and fenced into the grant). SQLite
-- ADD COLUMN with TEXT defaults (no network, pure-Go modernc dialect).

-- task_text: the imperative the agent must satisfy (from `flowbee seed -task` or
-- a GitHub issue body, §B). spec_text: the longer spec/design context. Both are
-- RESOLVED facts folded onto the job, never read from a clock.
ALTER TABLE jobs ADD COLUMN task_text TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN spec_text TEXT NOT NULL DEFAULT '';

-- acceptance_criteria: the DONE-WHEN the gate/agent checks against (newline list).
ALTER TABLE jobs ADD COLUMN acceptance_criteria TEXT NOT NULL DEFAULT '';
