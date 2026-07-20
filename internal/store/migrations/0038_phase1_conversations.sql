-- 0038: durable per-project human <-> Interactor conversations.
--
-- Conversation prose is durable presentation/context, never workflow authority.
-- Typed decisions and work intents remain separate, version-fenced models. The
-- message/event ledgers are append-only so reload, process restart, and SSE cursor
-- replay cannot lose or rewrite what either actor said.

CREATE TABLE conversation_threads (
    id                         TEXT PRIMARY KEY,
    project_id                 TEXT NOT NULL,
    conversation_key           TEXT NOT NULL,
    title                      TEXT NOT NULL DEFAULT '',
    interactor_actor_id        TEXT NOT NULL,
    interactor_binding_id      TEXT NOT NULL DEFAULT '',
    interactor_incarnation_id  TEXT NOT NULL DEFAULT '',
    state                      TEXT NOT NULL DEFAULT 'active'
                               CHECK (state IN ('active','archived')),
    state_version              INTEGER NOT NULL DEFAULT 1 CHECK (state_version > 0),
    focus_kind                 TEXT NOT NULL DEFAULT 'project'
                               CHECK (focus_kind IN ('project','epic','artifact','decision')),
    focus_ref                  TEXT NOT NULL,
    focus_artifact_sha256      TEXT NOT NULL DEFAULT '',
    last_message_seq           INTEGER NOT NULL DEFAULT 0 CHECK (last_message_seq >= 0),
    creation_idempotency_key   TEXT NOT NULL,
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    UNIQUE (project_id, conversation_key),
    UNIQUE (project_id, creation_idempotency_key)
);
CREATE INDEX idx_conversation_threads_project_state
    ON conversation_threads(project_id,state,updated_at,id);

CREATE TABLE conversation_messages (
    id                    TEXT PRIMARY KEY,
    project_id            TEXT NOT NULL,
    thread_id             TEXT NOT NULL,
    thread_seq            INTEGER NOT NULL CHECK (thread_seq > 0),
    role                  TEXT NOT NULL CHECK (role IN ('human','interactor','system')),
    actor_id              TEXT NOT NULL,
    agent_incarnation_id  TEXT NOT NULL DEFAULT '',
    reply_to_message_id   TEXT,
    content_text          TEXT NOT NULL DEFAULT '',
    content_artifact_ref  TEXT NOT NULL DEFAULT '',
    content_sha256        TEXT NOT NULL,
    stream_state          TEXT NOT NULL DEFAULT 'complete'
                          CHECK (stream_state IN ('complete','streaming')),
    idempotency_key       TEXT NOT NULL,
    created_at            TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id) ON DELETE CASCADE,
    FOREIGN KEY (reply_to_message_id) REFERENCES conversation_messages(id),
    UNIQUE (thread_id,thread_seq),
    UNIQUE (project_id,thread_id,idempotency_key)
);
CREATE INDEX idx_conversation_messages_thread_seq
    ON conversation_messages(project_id,thread_id,thread_seq);

-- Mutable transport state is deliberately separate from immutable message truth.
-- A replaceable Driver projector advances this projection and appends an event.
CREATE TABLE conversation_message_deliveries (
    message_id       TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    thread_id        TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending','routing','submitted','acknowledged',
                                      'uncertain','failed','fenced','not_required')),
    state_version    INTEGER NOT NULL DEFAULT 1 CHECK (state_version > 0),
    action_id        TEXT NOT NULL DEFAULT '',
    receipt_ref      TEXT NOT NULL DEFAULT '',
    last_error       TEXT NOT NULL DEFAULT '',
    updated_at       TEXT NOT NULL,
    FOREIGN KEY (message_id) REFERENCES conversation_messages(id) ON DELETE CASCADE,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id) ON DELETE CASCADE
);
CREATE INDEX idx_conversation_deliveries_state
    ON conversation_message_deliveries(state,updated_at);

-- Global seq is the durable SSE cursor. It is never an in-memory subscription
-- offset; a restarted server replays rows after Last-Event-ID.
CREATE TABLE conversation_events (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   TEXT NOT NULL,
    thread_id    TEXT NOT NULL,
    message_id   TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL CHECK (kind IN
                 ('thread_created','focus_changed','message_appended','delivery_changed')),
    payload_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json)),
    created_at   TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id) ON DELETE CASCADE
);
CREATE INDEX idx_conversation_events_thread_seq
    ON conversation_events(project_id,thread_id,seq);

-- Idempotency for stateful thread commands (focus and delivery transitions). The
-- original request hash/result is immutable; reusing a key with another body is a
-- conflict instead of silently applying a second mutation.
CREATE TABLE conversation_commands (
    project_id       TEXT NOT NULL,
    thread_id        TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    kind             TEXT NOT NULL,
    request_sha256   TEXT NOT NULL,
    result_ref       TEXT NOT NULL DEFAULT '',
    result_version   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    PRIMARY KEY (project_id,thread_id,idempotency_key),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id) ON DELETE CASCADE
);

CREATE TRIGGER conversation_messages_append_only_update
BEFORE UPDATE ON conversation_messages
BEGIN
    SELECT RAISE(ABORT, 'conversation_messages is append-only');
END;
CREATE TRIGGER conversation_messages_append_only_delete
BEFORE DELETE ON conversation_messages
BEGIN
    SELECT RAISE(ABORT, 'conversation_messages is append-only');
END;
CREATE TRIGGER conversation_events_append_only_update
BEFORE UPDATE ON conversation_events
BEGIN
    SELECT RAISE(ABORT, 'conversation_events is append-only');
END;
CREATE TRIGGER conversation_events_append_only_delete
BEFORE DELETE ON conversation_events
BEGIN
    SELECT RAISE(ABORT, 'conversation_events is append-only');
END;
CREATE TRIGGER conversation_commands_append_only_update
BEFORE UPDATE ON conversation_commands
BEGIN
    SELECT RAISE(ABORT, 'conversation_commands is append-only');
END;
CREATE TRIGGER conversation_commands_append_only_delete
BEFORE DELETE ON conversation_commands
BEGIN
    SELECT RAISE(ABORT, 'conversation_commands is append-only');
END;
