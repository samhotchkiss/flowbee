-- 0045: Phase-2 attribution for durable child records whose ownership was
-- historically reachable only by joining through a job or epic.
--
-- Defaults preserve old clients during the compatibility window. The triggers
-- are a backstop for legacy writers: new Phase-2 writers should still provide
-- project_id explicitly, but an omitted value can never silently attribute a
-- non-default project's child record to the default project.

ALTER TABLE leases ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE result_idempotency ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE timers ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE job_alarms ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE epoch_ci ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE compensations ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE attention_items ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE wip_markers ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';

UPDATE leases SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=leases.job_id),'default');
UPDATE result_idempotency SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=result_idempotency.job_id),'default');
UPDATE timers SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=timers.job_id),'default');
UPDATE job_alarms SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=job_alarms.job_id),'default');
UPDATE epoch_ci SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=epoch_ci.job_id),'default');
UPDATE compensations SET project_id=COALESCE((SELECT project_id FROM jobs WHERE jobs.id=compensations.job_id),'default');
UPDATE attention_items SET project_id=COALESCE((SELECT project_id FROM epics WHERE epics.id=attention_items.epic_id),'default');
UPDATE wip_markers SET project_id=COALESCE((SELECT project_id FROM epics WHERE epics.id=wip_markers.epic_id),'default');

CREATE INDEX idx_leases_project_job ON leases(project_id,job_id,lease_epoch);
CREATE INDEX idx_result_idempotency_project ON result_idempotency(project_id,job_id);
CREATE INDEX idx_timers_project_due ON timers(project_id,fired,due_at);
CREATE INDEX idx_job_alarms_project ON job_alarms(project_id,job_id,kind);
CREATE INDEX idx_epoch_ci_project ON epoch_ci(project_id,job_id,epoch);
CREATE INDEX idx_compensations_project ON compensations(project_id,job_id,dead_epoch);
CREATE INDEX idx_attention_project_state ON attention_items(project_id,state,priority);
CREATE INDEX idx_wip_project_open ON wip_markers(project_id,cleared_at,epic_id);

CREATE TRIGGER leases_project_owner_after_insert
AFTER INSERT ON leases
BEGIN
  UPDATE leases SET project_id=COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')
   WHERE lease_id=NEW.lease_id;
END;

CREATE TRIGGER result_idempotency_project_owner_after_insert
AFTER INSERT ON result_idempotency
BEGIN
  UPDATE result_idempotency SET project_id=COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')
   WHERE job_id=NEW.job_id AND idempotency_key=NEW.idempotency_key;
END;

CREATE TRIGGER timers_project_owner_after_insert
AFTER INSERT ON timers
BEGIN
  UPDATE timers SET project_id=COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')
   WHERE id=NEW.id;
END;

CREATE TRIGGER job_alarms_project_owner_after_insert
AFTER INSERT ON job_alarms
BEGIN
  UPDATE job_alarms SET project_id=COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')
   WHERE job_id=NEW.job_id AND kind=NEW.kind;
END;

CREATE TRIGGER epoch_ci_project_owner_after_insert
AFTER INSERT ON epoch_ci
BEGIN
  UPDATE epoch_ci SET project_id=COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')
   WHERE job_id=NEW.job_id AND epoch=NEW.epoch;
END;

CREATE TRIGGER compensations_project_owner_after_insert
AFTER INSERT ON compensations
BEGIN
  UPDATE compensations SET project_id=COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')
   WHERE job_id=NEW.job_id AND dead_epoch=NEW.dead_epoch;
END;

CREATE TRIGGER attention_project_owner_after_insert
AFTER INSERT ON attention_items WHEN NEW.epic_id<>''
BEGIN
  UPDATE attention_items SET project_id=COALESCE((SELECT project_id FROM epics WHERE id=NEW.epic_id),'default')
   WHERE id=NEW.id;
END;

CREATE TRIGGER wip_project_owner_after_insert
AFTER INSERT ON wip_markers
BEGIN
  UPDATE wip_markers SET project_id=COALESCE((SELECT project_id FROM epics WHERE id=NEW.epic_id),'default')
   WHERE id=NEW.id;
END;
