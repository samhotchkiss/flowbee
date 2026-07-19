-- 0053: route Flowbee control alerts to the exact current project Interactor.
--
-- The alert remains workflow truth.  This table is the immutable join to the
-- conversation message created for one exact logical actor-route version and
-- Driver binding.  A transport receipt cannot acknowledge the alert: the
-- acknowledgement trigger below requires separately recorded processing
-- evidence for the exact conversation action epoch.

CREATE TABLE control_alert_interactor_projections (
    control_alert_id             TEXT PRIMARY KEY,
    project_id                   TEXT NOT NULL,
    project_actor_id             TEXT NOT NULL,
    project_actor_route_version  INTEGER NOT NULL CHECK (project_actor_route_version > 0),
    target_binding_id            TEXT NOT NULL,
    target_binding_epoch         INTEGER NOT NULL CHECK (target_binding_epoch > 0),
    thread_id                    TEXT NOT NULL,
    message_id                   TEXT NOT NULL UNIQUE,
    created_at                   TEXT NOT NULL,
    FOREIGN KEY (control_alert_id) REFERENCES control_alerts(id),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (target_binding_id) REFERENCES driver_session_bindings(binding_id),
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id),
    FOREIGN KEY (message_id) REFERENCES conversation_messages(id)
);
CREATE INDEX idx_control_alert_interactor_projection_route
    ON control_alert_interactor_projections
       (project_id,project_actor_id,project_actor_route_version,target_binding_epoch);

CREATE TRIGGER control_alert_interactor_projections_immutable_update
BEFORE UPDATE ON control_alert_interactor_projections
BEGIN
    SELECT RAISE(ABORT, 'control alert Interactor projection is immutable');
END;
CREATE TRIGGER control_alert_interactor_projections_immutable_delete
BEFORE DELETE ON control_alert_interactor_projections
BEGIN
    SELECT RAISE(ABORT, 'control alert Interactor projection is immutable');
END;

-- A linked system notification is not allowed to claim processing merely
-- because a client (or a Driver receipt) advanced its delivery projection.  A
-- confirmed post-baseline evidence row for the exact immutable action epoch is
-- mandatory before acknowledged can be committed.
CREATE TRIGGER control_alert_interactor_ack_requires_evidence
BEFORE UPDATE OF state ON conversation_message_deliveries
WHEN NEW.state='acknowledged' AND OLD.state<>'acknowledged'
 AND EXISTS (SELECT 1 FROM control_alert_interactor_projections p
              WHERE p.message_id=NEW.message_id)
 AND NOT EXISTS (
     SELECT 1 FROM conversation_message_actions a
     JOIN conversation_message_action_evidence e
       ON e.action_id=a.id AND e.action_epoch=a.action_epoch
     JOIN driver_session_bindings b ON b.binding_id=a.target_binding_id
     WHERE a.id=NEW.action_id AND a.message_id=NEW.message_id
       AND a.state='acknowledged' AND e.state='confirmed'
       AND e.store_seq>a.evidence_baseline_store_seq
       AND e.store_id=b.store_id AND e.session_id=b.session_id
       AND e.pane_instance_id=b.pane_instance_id
       AND e.agent_run_id=b.agent_run_id
 )
BEGIN
    SELECT RAISE(ABORT, 'control alert acknowledgement requires Interactor processing evidence');
END;

-- The source alert is acknowledged only after the existing conversation
-- evidence projector reaches acknowledged.  Merely projecting or submitting
-- the message leaves it in the distinct projected state and visible.
CREATE TRIGGER control_alert_interactor_delivery_acknowledged
AFTER UPDATE OF state ON conversation_message_deliveries
WHEN NEW.state='acknowledged' AND OLD.state<>'acknowledged'
BEGIN
    UPDATE control_alerts
       SET state='acknowledged', acknowledged_at=NEW.updated_at,
           claim_owner='', claim_deadline_at='', next_attempt_at='',
           last_error='', updated_at=NEW.updated_at
     WHERE id=(SELECT control_alert_id
                 FROM control_alert_interactor_projections
                WHERE message_id=NEW.message_id)
       AND state='projected';

    UPDATE epic_deliveries
       SET alert_pending=0, updated_at=NEW.updated_at
     WHERE epic_id=(SELECT a.epic_id FROM control_alerts a
                     JOIN control_alert_interactor_projections p
                       ON p.control_alert_id=a.id
                    WHERE p.message_id=NEW.message_id)
       AND NOT EXISTS (
           SELECT 1 FROM control_alerts pending
            WHERE pending.epic_id=epic_deliveries.epic_id
              AND pending.state IN ('pending','delivering','projected')
       );
END;
