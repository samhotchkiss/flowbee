-- 0051: durable authority for externally adopted Driver sessions.
--
-- Raw tmux pane selectors are bootstrap-only and are intentionally absent.
-- Empty defaults leave every pre-migration row unadopted and fail closed.
ALTER TABLE driver_session_bindings ADD COLUMN external_watch_id TEXT NOT NULL DEFAULT '';
ALTER TABLE epic_actions ADD COLUMN external_watch_id TEXT NOT NULL DEFAULT '';
ALTER TABLE epic_actions ADD COLUMN sender_host_id TEXT NOT NULL DEFAULT '';
ALTER TABLE epic_actions ADD COLUMN sender_store_id TEXT NOT NULL DEFAULT '';
ALTER TABLE epic_actions ADD COLUMN sender_server_domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE epic_actions ADD COLUMN sender_server_id TEXT NOT NULL DEFAULT '';

-- Control-origin v2 grants and receipts must retain the exact recipient run
-- fence. Session-origin v1 rows must not acquire that field by inference.
ALTER TABLE driver_grants ADD COLUMN expected_recipient_agent_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE driver_receipts ADD COLUMN expected_recipient_agent_run_id TEXT NOT NULL DEFAULT '';

-- Lifecycle receipt identity is not reconstructible from JSON after the fact:
-- the selected server domain and external watch are top-level canonical fences.
ALTER TABLE driver_lifecycle_receipts ADD COLUMN tmux_server_domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE driver_lifecycle_receipts ADD COLUMN external_watch_id TEXT NOT NULL DEFAULT '';

CREATE TRIGGER driver_grants_recipient_run_fence_insert
BEFORE INSERT ON driver_grants
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.expected_recipient_agent_run_id<>'') OR
    (NEW.sender_principal_id='' AND NEW.expected_recipient_agent_run_id='')
)
BEGIN
    SELECT RAISE(ABORT,'driver grant recipient run fence does not match origin contract');
END;

-- The compatibility session-origin route is legal only within the exact same
-- endpoint tuple as its recipient. Product/control-origin actions carry no
-- session endpoint tuple at all.
CREATE TRIGGER epic_actions_sender_endpoint_fence_insert
BEFORE INSERT ON epic_actions
WHEN NEW.executor_kind='driver' AND NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_host_id='' AND NEW.sender_store_id='' AND
     NEW.sender_server_domain_id='' AND NEW.sender_server_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_host_id<>'' AND NEW.sender_store_id<>'' AND
     NEW.sender_server_domain_id<>'' AND NEW.sender_server_id<>'' AND
     NEW.sender_host_id=NEW.target_host_id AND NEW.sender_store_id=NEW.target_store_id AND
     NEW.sender_server_domain_id=NEW.target_server_domain_id AND NEW.sender_server_id=NEW.target_server_id)
)
BEGIN
    SELECT RAISE(ABORT,'driver session-origin action crosses endpoint boundary');
END;
CREATE TRIGGER epic_actions_sender_endpoint_fence_update
BEFORE UPDATE OF executor_kind,sender_principal_id,sender_host_id,sender_store_id,sender_server_domain_id,
    sender_server_id,target_host_id,target_store_id,target_server_domain_id,target_server_id ON epic_actions
WHEN NEW.executor_kind='driver' AND NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_host_id='' AND NEW.sender_store_id='' AND
     NEW.sender_server_domain_id='' AND NEW.sender_server_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_host_id<>'' AND NEW.sender_store_id<>'' AND
     NEW.sender_server_domain_id<>'' AND NEW.sender_server_id<>'' AND
     NEW.sender_host_id=NEW.target_host_id AND NEW.sender_store_id=NEW.target_store_id AND
     NEW.sender_server_domain_id=NEW.target_server_domain_id AND NEW.sender_server_id=NEW.target_server_id)
)
BEGIN
    SELECT RAISE(ABORT,'driver session-origin action crosses endpoint boundary');
END;
CREATE TRIGGER driver_grants_recipient_run_fence_update
BEFORE UPDATE OF sender_principal_id,expected_recipient_agent_run_id ON driver_grants
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.expected_recipient_agent_run_id<>'') OR
    (NEW.sender_principal_id='' AND NEW.expected_recipient_agent_run_id='')
)
BEGIN
    SELECT RAISE(ABORT,'driver grant recipient run fence does not match origin contract');
END;

CREATE TRIGGER driver_receipts_recipient_run_fence_insert
BEFORE INSERT ON driver_receipts
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.expected_recipient_agent_run_id<>'') OR
    (NEW.sender_principal_id='' AND NEW.expected_recipient_agent_run_id='')
)
BEGIN
    SELECT RAISE(ABORT,'driver receipt recipient run fence does not match origin contract');
END;
CREATE TRIGGER driver_receipts_recipient_run_fence_update
BEFORE UPDATE OF sender_principal_id,expected_recipient_agent_run_id ON driver_receipts
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.expected_recipient_agent_run_id<>'') OR
    (NEW.sender_principal_id='' AND NEW.expected_recipient_agent_run_id='')
)
BEGIN
    SELECT RAISE(ABORT,'driver receipt recipient run fence does not match origin contract');
END;

CREATE TRIGGER driver_lifecycle_receipts_v25_fences_insert
BEFORE INSERT ON driver_lifecycle_receipts
WHEN NEW.tmux_server_domain_id='' OR NOT (
    (NEW.operation IN ('adopt','release') AND NEW.external_watch_id<>'') OR
	(NEW.operation='reattach') OR
    (NEW.operation NOT IN ('adopt','release','reattach') AND NEW.external_watch_id='')
)
BEGIN
    SELECT RAISE(ABORT,'driver lifecycle receipt lacks exact v2.5 domain/watch fences');
END;
CREATE TRIGGER driver_lifecycle_receipts_v25_fences_update
BEFORE UPDATE OF operation,tmux_server_domain_id,external_watch_id ON driver_lifecycle_receipts
WHEN NEW.tmux_server_domain_id='' OR NOT (
    (NEW.operation IN ('adopt','release') AND NEW.external_watch_id<>'') OR
	(NEW.operation='reattach') OR
    (NEW.operation NOT IN ('adopt','release','reattach') AND NEW.external_watch_id='')
)
BEGIN
    SELECT RAISE(ABORT,'driver lifecycle receipt lacks exact v2.5 domain/watch fences');
END;
