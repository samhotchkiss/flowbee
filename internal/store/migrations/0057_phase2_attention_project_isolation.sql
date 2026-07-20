-- 0057: make the durable attention queue project-local.
--
-- Epic-backed rows were already repaired by 0045. Epic-less capacity and work
-- intent rows carry their owning project in immutable evidence; recover those
-- before replacing the legacy global active-dedup index. Rows without durable
-- project evidence remain explicit legacy/default records.

UPDATE attention_items
   SET project_id=CASE WHEN json_valid(evidence_json)
                       THEN COALESCE((SELECT id FROM projects
                                      WHERE id=json_extract(attention_items.evidence_json,'$.project_id')),
                                     project_id)
                       ELSE project_id END
 WHERE epic_id=''
   AND project_id='default';

DROP INDEX IF EXISTS idx_attention_active_dedup;

CREATE UNIQUE INDEX idx_attention_active_project_dedup
    ON attention_items(project_id,dedup_key)
 WHERE state IN ('open','leased','delivering','awaiting_ack');

CREATE INDEX idx_attention_project_kind_state
    ON attention_items(project_id,kind,state,priority);

CREATE TRIGGER attention_project_valid_insert
BEFORE INSERT ON attention_items
WHEN NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.project_id)
BEGIN
  SELECT RAISE(ABORT,'attention project does not exist');
END;

-- 0045's compatibility trigger repairs a legacy epic writer that omitted the
-- project column (default -> immutable epic owner). That exact repair is the only
-- allowed ownership mutation; callers cannot rebind an item, its epic, or dedup
-- identity after insertion.
CREATE TRIGGER attention_project_identity_immutable
BEFORE UPDATE OF project_id,epic_id,dedup_key ON attention_items
WHEN (OLD.project_id<>NEW.project_id OR OLD.epic_id<>NEW.epic_id OR OLD.dedup_key<>NEW.dedup_key)
 AND NOT (OLD.project_id='default'
          AND OLD.epic_id=NEW.epic_id
          AND OLD.dedup_key=NEW.dedup_key
          AND NEW.epic_id<>''
          AND NEW.project_id=(SELECT project_id FROM epics WHERE id=NEW.epic_id))
BEGIN
  SELECT RAISE(ABORT,'attention project identity is immutable');
END;
