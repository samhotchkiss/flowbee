-- 0050: v2.5 tmux server-domain fencing.
--
-- Empty defaults are deliberately fail-closed. Existing rows do not acquire a
-- domain or lifecycle ownership by inference; a fresh strict Driver metadata or
-- lifecycle receipt must establish those fields before the row can authorize a
-- new effect.
ALTER TABLE driver_instances ADD COLUMN tmux_server_domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE driver_instances ADD COLUMN tmux_server_ownership TEXT NOT NULL DEFAULT '';

ALTER TABLE builder_driver_targets ADD COLUMN tmux_server_domain_id TEXT NOT NULL DEFAULT '';

ALTER TABLE driver_session_bindings ADD COLUMN tmux_server_domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE driver_session_bindings ADD COLUMN lifecycle_ownership TEXT NOT NULL DEFAULT '';

ALTER TABLE epic_actions ADD COLUMN target_server_domain_id TEXT NOT NULL DEFAULT '';

ALTER TABLE driver_session_projections ADD COLUMN tmux_server_domain_id TEXT NOT NULL DEFAULT '';
