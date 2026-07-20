-- 0067: distinguish a certified local pre-effect worker Ensure failure from a
-- Driver delivery whose outcome is unknown.  This is deliberately a separate
-- append-only fact rather than an interpretation of an action's retry state:
-- merge cleanup may only tear down a prepared local worktree when it can prove
-- that Driver was never invoked for the immutable Ensure action.

CREATE TABLE epic_lifecycle_pre_effect_failures (
    action_id       TEXT PRIMARY KEY,
    action_epoch    INTEGER NOT NULL CHECK (action_epoch > 0),
    failure_kind    TEXT NOT NULL CHECK (failure_kind IN ('workspace_prepare','materials_resolve','launch_validate')),
    reason          TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    FOREIGN KEY (action_id) REFERENCES epic_actions(id) ON DELETE CASCADE
);

CREATE TRIGGER trg_epic_lifecycle_pre_effect_failure_immutable
BEFORE UPDATE ON epic_lifecycle_pre_effect_failures
WHEN NEW.action_id<>OLD.action_id OR NEW.action_epoch<>OLD.action_epoch
  OR NEW.failure_kind<>OLD.failure_kind OR NEW.reason<>OLD.reason
  OR NEW.recorded_at<>OLD.recorded_at
BEGIN
    SELECT RAISE(ABORT,'epic lifecycle pre-effect failure is immutable');
END;

ALTER TABLE epic_worker_sessions ADD COLUMN cleanup_action_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_epic_worker_cleanup_action
    ON epic_worker_sessions(cleanup_action_id) WHERE cleanup_action_id<>'';
