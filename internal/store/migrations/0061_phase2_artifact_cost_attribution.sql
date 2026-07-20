-- 0061: harden the existing artifact and cost ledgers for project isolation.
--
-- epic_artifacts is the active artifact projection; job cost columns plus
-- cost-bearing job_events are the active cost ledger. Do not create parallel
-- artifact/cost tables. Repair compatibility-default rows from their immutable
-- parent, then fence future spoofing and ownership moves.

UPDATE epic_artifacts
   SET project_id=COALESCE((SELECT project_id FROM epics WHERE epics.id=epic_artifacts.epic_id),'default');

UPDATE job_events
   SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=job_events.job_id),'default');

CREATE INDEX idx_epic_artifacts_project_epic
    ON epic_artifacts(project_id,epic_id);

CREATE TRIGGER epic_artifact_project_owner_insert
BEFORE INSERT ON epic_artifacts
WHEN (EXISTS (SELECT 1 FROM epics WHERE id=NEW.epic_id)
      AND NOT EXISTS (SELECT 1 FROM epics WHERE id=NEW.epic_id AND project_id=NEW.project_id))
  OR (NOT EXISTS (SELECT 1 FROM epics WHERE id=NEW.epic_id) AND NEW.project_id<>'default')
BEGIN
  SELECT RAISE(ABORT,'epic artifact project ownership mismatch');
END;

CREATE TRIGGER epic_artifact_project_identity_immutable
BEFORE UPDATE OF epic_id,project_id ON epic_artifacts
WHEN OLD.epic_id<>NEW.epic_id OR OLD.project_id<>NEW.project_id
BEGIN
  SELECT RAISE(ABORT,'epic artifact project identity is immutable');
END;

CREATE TRIGGER epic_project_immutable_after_artifact
BEFORE UPDATE OF project_id ON epics
WHEN OLD.project_id<>NEW.project_id
 AND EXISTS (SELECT 1 FROM epic_artifacts WHERE epic_id=OLD.id)
BEGIN
  SELECT RAISE(ABORT,'epic project is immutable after artifact attribution');
END;

CREATE TRIGGER job_event_project_owner_insert
BEFORE INSERT ON job_events
WHEN (EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id)
      AND NOT EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id AND project_id=NEW.project_id))
  OR (NOT EXISTS (SELECT 1 FROM jobs WHERE id=NEW.job_id) AND NEW.project_id<>'default')
BEGIN
  SELECT RAISE(ABORT,'job event project ownership mismatch');
END;

CREATE TRIGGER job_event_project_identity_immutable
BEFORE UPDATE OF job_id,project_id ON job_events
WHEN OLD.job_id<>NEW.job_id OR OLD.project_id<>NEW.project_id
BEGIN
  SELECT RAISE(ABORT,'job event project identity is immutable');
END;

-- A job can retain the legacy pre-effect assignment window, but once any cost
-- has been metered its owning project is immutable. Both the projection and
-- replay evidence participate so a zero-dollar/token report is still fenced.
CREATE TRIGGER job_project_immutable_after_cost
BEFORE UPDATE OF project_id ON jobs
WHEN OLD.project_id<>NEW.project_id AND (
  OLD.cost_tokens_in<>0 OR OLD.cost_tokens_out<>0 OR OLD.cost_micro_usd<>0
  OR EXISTS (SELECT 1 FROM job_events
              WHERE job_id=OLD.id AND kind IN ('cost_metered','cost_escalated'))
)
BEGIN
  SELECT RAISE(ABORT,'job project is immutable after cost attribution');
END;
