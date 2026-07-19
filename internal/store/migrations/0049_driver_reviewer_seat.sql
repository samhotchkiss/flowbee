-- 0049: bind each project reviewer route to one exact authenticated capacity
-- seat. Existing rows retain the empty compatibility value and fail project
-- activation closed until they are explicitly re-onboarded.
ALTER TABLE driver_session_bindings ADD COLUMN seat_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_driver_session_bindings_seat
    ON driver_session_bindings(project_id, role, seat_id)
    WHERE state='active' AND seat_id<>'';
