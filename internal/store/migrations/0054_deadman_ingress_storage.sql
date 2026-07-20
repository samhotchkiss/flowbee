-- 0054: durable, body-bound acceptance for signed external dead-man alerts.
--
-- The ingress row and its project control_alert are committed in one SQLite
-- transaction.  The exact request bytes are retained so a signed HTTP
-- acknowledgement can mean durable acceptance, while a reused idempotency key
-- with even one changed byte is a conflict rather than a second alert.

CREATE TABLE control_alert_ingress_submissions (
    idempotency_key  TEXT PRIMARY KEY,
    body_sha256      TEXT NOT NULL,
    body             BLOB NOT NULL,
    project_id       TEXT NOT NULL,
    control_alert_id TEXT NOT NULL UNIQUE,
    envelope_id      TEXT NOT NULL,
    envelope_kind    TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    CHECK (length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (length(body_sha256)=64 AND body_sha256=lower(body_sha256)),
    CHECK (length(body)>0),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (control_alert_id) REFERENCES control_alerts(id)
);
CREATE INDEX idx_control_alert_ingress_project_created
    ON control_alert_ingress_submissions(project_id,created_at,idempotency_key);

CREATE TRIGGER control_alert_ingress_submissions_immutable_update
BEFORE UPDATE ON control_alert_ingress_submissions
BEGIN
    SELECT RAISE(ABORT, 'control alert ingress submission is immutable');
END;
CREATE TRIGGER control_alert_ingress_submissions_immutable_delete
BEFORE DELETE ON control_alert_ingress_submissions
BEGIN
    SELECT RAISE(ABORT, 'control alert ingress submission is immutable');
END;

-- Delivery state remains mutable, but the alert identity and exact payload
-- linked to authenticated ingress cannot be rewritten after acknowledgement.
CREATE TRIGGER control_alert_ingress_alert_identity_immutable
BEFORE UPDATE ON control_alerts
WHEN EXISTS (SELECT 1 FROM control_alert_ingress_submissions i
              WHERE i.control_alert_id=OLD.id)
 AND (NEW.id<>OLD.id OR NEW.project_id<>OLD.project_id
      OR NEW.epic_id IS NOT OLD.epic_id OR NEW.kind<>OLD.kind
      OR NEW.dedup_key<>OLD.dedup_key OR NEW.payload_json<>OLD.payload_json)
BEGIN
    SELECT RAISE(ABORT, 'ingress control alert identity is immutable');
END;
