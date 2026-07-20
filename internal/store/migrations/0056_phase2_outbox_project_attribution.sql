-- 0056: make the legacy GitHub outbox and its audit ledger project-owned.
--
-- 0043 added project_id with a default-project compatibility default, but the
-- production writers continued to omit the column.  That silently attributed
-- actions for non-default jobs to `default`.  Repair existing ownership from
-- the immutable job parent, then reject every future cross-project identity.
-- Orphaned legacy/default rows remain readable so upgrading an old install is
-- non-destructive; new Store writes require a real owning job.

UPDATE outbox
   SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=outbox.job_id),'default');

UPDATE audit_log
   SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=audit_log.job_id),'default');

CREATE TRIGGER outbox_project_owner_insert
BEFORE INSERT ON outbox
WHEN (EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id)
      AND NOT EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id AND project_id=NEW.project_id))
  OR (NOT EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id) AND NEW.project_id<>'default')
BEGIN
  SELECT RAISE(ABORT,'outbox project ownership mismatch');
END;

CREATE TRIGGER outbox_project_identity_immutable
BEFORE UPDATE OF job_id,project_id,action,head_sha ON outbox
WHEN OLD.job_id<>NEW.job_id OR OLD.project_id<>NEW.project_id
  OR OLD.action<>NEW.action OR OLD.head_sha<>NEW.head_sha
BEGIN
  SELECT RAISE(ABORT,'outbox project identity is immutable');
END;

CREATE TRIGGER audit_log_project_owner_insert
BEFORE INSERT ON audit_log
WHEN (EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id)
      AND NOT EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id AND project_id=NEW.project_id))
  OR (NOT EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id) AND NEW.project_id<>'default')
  OR (NEW.project_id<>'default' AND NOT EXISTS (
        SELECT 1 FROM outbox
         WHERE job_id=NEW.job_id AND action=NEW.action AND head_sha=NEW.head_sha
           AND project_id=NEW.project_id))
BEGIN
  SELECT RAISE(ABORT,'audit log project ownership mismatch');
END;

CREATE TRIGGER audit_log_project_identity_immutable
BEFORE UPDATE OF job_id,project_id,action,head_sha ON audit_log
WHEN OLD.job_id<>NEW.job_id OR OLD.project_id<>NEW.project_id
  OR OLD.action<>NEW.action OR OLD.head_sha<>NEW.head_sha
BEGIN
  SELECT RAISE(ABORT,'audit log project identity is immutable');
END;

-- Once a durable effect exists, moving its parent job to another project would
-- rewrite authority underneath an immutable audit identity.  A job may still
-- be assigned during legacy admission before its first effect is enqueued.
CREATE TRIGGER job_project_immutable_after_outbox
BEFORE UPDATE OF project_id ON jobs
WHEN OLD.project_id<>NEW.project_id AND (
  EXISTS (SELECT 1 FROM outbox WHERE job_id=OLD.id)
  OR EXISTS (SELECT 1 FROM audit_log WHERE job_id=OLD.id))
BEGIN
  SELECT RAISE(ABORT,'job project is immutable after outbox attribution');
END;
