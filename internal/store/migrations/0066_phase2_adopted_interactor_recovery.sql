-- 0066: durable managed fallback for an adopted external Interactor.
--
-- Adoption remains observation-only.  The recovery policy is committed with
-- the adopt intent so an exact later absence can atomically fence the external
-- incarnation and enqueue a v3 managed Ensure without consulting CWD, a tmux
-- name, or process memory.

CREATE TABLE project_actor_managed_recovery_policies (
    project_id                       TEXT NOT NULL,
    role                             TEXT NOT NULL CHECK (role='interactor'),
    actor_id                         TEXT NOT NULL,
    profile_id                       TEXT NOT NULL CHECK (profile_id='claude_interactor_managed'),
    workspace_root_id                TEXT NOT NULL,
    workspace_relative_path          TEXT NOT NULL,
    source_intent_payload_sha256      TEXT NOT NULL,
    created_at                       TEXT NOT NULL,
    updated_at                       TEXT NOT NULL,
    PRIMARY KEY (project_id,role,actor_id),
    FOREIGN KEY (project_id,role,actor_id)
        REFERENCES project_actor_lifecycles(project_id,role,actor_id) ON DELETE CASCADE
);

CREATE TRIGGER trg_project_actor_managed_recovery_policy_shape
BEFORE INSERT ON project_actor_managed_recovery_policies
WHEN NEW.project_id='' OR NEW.actor_id='' OR NEW.workspace_root_id=''
  OR NEW.workspace_relative_path='' OR NEW.source_intent_payload_sha256 NOT GLOB 'sha256:*'
BEGIN
    SELECT RAISE(ABORT,'invalid project actor managed recovery policy');
END;

CREATE TRIGGER trg_project_actor_managed_recovery_policy_immutable
BEFORE UPDATE OF project_id,role,actor_id,profile_id,workspace_root_id,
    workspace_relative_path,source_intent_payload_sha256
ON project_actor_managed_recovery_policies
WHEN NEW.project_id<>OLD.project_id OR NEW.role<>OLD.role OR NEW.actor_id<>OLD.actor_id
  OR NEW.profile_id<>OLD.profile_id OR NEW.workspace_root_id<>OLD.workspace_root_id
  OR NEW.workspace_relative_path<>OLD.workspace_relative_path
  OR NEW.source_intent_payload_sha256<>OLD.source_intent_payload_sha256
BEGIN
    SELECT RAISE(ABORT,'project actor managed recovery policy is immutable');
END;
