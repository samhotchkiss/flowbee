-- 0041: short-lived, one-time dashboard login exchanges.
-- Raw bearer material is never stored; the browser sends it from a URL fragment
-- to a POST endpoint, and the serialized consume fences replay.

CREATE TABLE human_login_tokens (
    token_sha256 TEXT PRIMARY KEY,
    identity     TEXT NOT NULL,
    session_id   TEXT NOT NULL UNIQUE,
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','consumed','expired')),
    expires_at   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    consumed_at  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_human_login_tokens_pending_expiry
    ON human_login_tokens(state, expires_at);
