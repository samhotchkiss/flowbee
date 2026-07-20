-- 0065: immutable Q3 launch material for Flowbee-created project actors.
-- Public bootstrap bytes and non-secret credential-envelope identity are bound
-- into both desired lifecycle state and the exact action. Plaintext credentials
-- remain only in the owner-only one-shot envelope outside SQLite.

ALTER TABLE project_actor_lifecycles ADD COLUMN bootstrap_format TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN bootstrap_payload TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN bootstrap_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_install_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_generation INTEGER NOT NULL DEFAULT 0 CHECK (credential_generation >= 0);
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_envelope_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_payload_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_envelope_deleted_at TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN credential_revoked_at TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycles ADD COLUMN presentation_name TEXT NOT NULL DEFAULT '';

ALTER TABLE project_actor_lifecycle_actions ADD COLUMN bootstrap_format TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN bootstrap_payload TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN bootstrap_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN credential_install_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN credential_generation INTEGER NOT NULL DEFAULT 0 CHECK (credential_generation >= 0);
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN credential_envelope_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN credential_payload_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN credential_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE project_actor_lifecycle_actions ADD COLUMN presentation_name TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_project_actor_credential_envelope
    ON project_actor_lifecycle_actions(project_id,credential_envelope_ref)
 WHERE credential_envelope_ref<>'' AND operation='ensure';

CREATE TRIGGER trg_project_actor_q3_action_immutable
BEFORE UPDATE OF bootstrap_format,bootstrap_payload,bootstrap_sha256,credential_install_ref,
    credential_generation,credential_envelope_ref,credential_payload_sha256,
    credential_expires_at,presentation_name
ON project_actor_lifecycle_actions
WHEN NEW.bootstrap_format<>OLD.bootstrap_format OR NEW.bootstrap_payload<>OLD.bootstrap_payload
  OR NEW.bootstrap_sha256<>OLD.bootstrap_sha256
  OR NEW.credential_install_ref<>OLD.credential_install_ref
  OR NEW.credential_generation<>OLD.credential_generation
  OR NEW.credential_envelope_ref<>OLD.credential_envelope_ref
  OR NEW.credential_payload_sha256<>OLD.credential_payload_sha256
  OR NEW.credential_expires_at<>OLD.credential_expires_at
  OR NEW.presentation_name<>OLD.presentation_name
BEGIN
    SELECT RAISE(ABORT,'project actor Q3 action material is immutable');
END;

CREATE TRIGGER trg_project_actor_q3_action_shape
BEFORE INSERT ON project_actor_lifecycle_actions
WHEN
  (NEW.operation='ensure' AND NOT (
      NEW.lifecycle_ownership='driver_managed'
      AND NEW.bootstrap_format='initial_prompt_utf8/v1'
      AND NEW.bootstrap_payload<>'' AND NEW.bootstrap_sha256 GLOB 'sha256:*'
      AND NEW.credential_install_ref<>'' AND NEW.credential_generation>0
      AND NEW.credential_generation=NEW.target_epoch
      AND NEW.credential_envelope_ref<>'' AND NEW.credential_payload_sha256 GLOB 'sha256:*'
      AND NEW.credential_expires_at<>'' AND NEW.presentation_name<>''))
  OR (NEW.operation IN ('adopt','release') AND NOT (
      NEW.bootstrap_format='' AND NEW.bootstrap_payload='' AND NEW.bootstrap_sha256=''
      AND NEW.credential_install_ref='' AND NEW.credential_generation=0
      AND NEW.credential_envelope_ref='' AND NEW.credential_payload_sha256=''
      AND NEW.credential_expires_at='' AND NEW.presentation_name=''))
  OR (NEW.operation IN ('stop','reattach') AND NOT (
      (NEW.lifecycle_ownership='external_observed' AND NEW.bootstrap_format=''
       AND NEW.bootstrap_payload='' AND NEW.bootstrap_sha256=''
       AND NEW.credential_install_ref='' AND NEW.credential_generation=0
       AND NEW.credential_envelope_ref='' AND NEW.credential_payload_sha256=''
       AND NEW.credential_expires_at='' AND NEW.presentation_name='')
      OR
      (NEW.lifecycle_ownership='driver_managed' AND NEW.bootstrap_format=''
       AND NEW.bootstrap_payload='' AND NEW.bootstrap_sha256=''
       AND NEW.credential_install_ref<>'' AND NEW.credential_generation>0
       AND NEW.credential_envelope_ref<>'' AND NEW.credential_payload_sha256 GLOB 'sha256:*'
       AND NEW.credential_expires_at<>'' AND NEW.presentation_name<>'')
      OR
      (NEW.operation='stop' AND NEW.lifecycle_ownership='driver_managed'
       AND NEW.bootstrap_format='' AND NEW.bootstrap_payload='' AND NEW.bootstrap_sha256=''
       AND NEW.credential_install_ref='' AND NEW.credential_generation=0
       AND NEW.credential_envelope_ref='' AND NEW.credential_payload_sha256=''
       AND NEW.credential_expires_at='' AND NEW.presentation_name=''
       AND EXISTS (
           SELECT 1 FROM project_actor_lifecycles AS parent
            WHERE parent.project_id=NEW.project_id AND parent.role=NEW.role
              AND parent.actor_id=NEW.actor_id AND parent.credential_generation=0
       ))))
BEGIN
    SELECT RAISE(ABORT,'invalid project actor Q3 action material shape');
END;

CREATE TRIGGER trg_project_actor_q3_target_credential_epoch
BEFORE UPDATE OF target_epoch,credential_generation ON project_actor_lifecycles
WHEN NEW.target_epoch>OLD.target_epoch AND NEW.credential_generation<=OLD.credential_generation
BEGIN
    SELECT RAISE(ABORT,'project actor replacement did not advance credential generation');
END;

CREATE TRIGGER trg_project_actor_q3_credential_no_regression
BEFORE UPDATE OF credential_generation ON project_actor_lifecycles
WHEN NEW.credential_generation < OLD.credential_generation
BEGIN
    SELECT RAISE(ABORT,'project actor credential generation regressed');
END;
