-- 0059: durable fail-closed repository-to-project admission routing holds.
-- Admission may proceed only when one active project owns the active repository.
-- Zero or multiple owners are persisted here instead of silently choosing the
-- compatibility default project.

CREATE TABLE repo_admission_holds (
    repo_id                 TEXT PRIMARY KEY,
    state                   TEXT NOT NULL CHECK (state IN ('pending','resolved')),
    reason                  TEXT NOT NULL,
    candidate_projects_json TEXT NOT NULL DEFAULT '[]',
    occurrences             INTEGER NOT NULL DEFAULT 1,
    first_seen_at           TEXT NOT NULL,
    last_seen_at            TEXT NOT NULL,
    resolved_at             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_repo_admission_holds_state
    ON repo_admission_holds(state,last_seen_at,repo_id);
