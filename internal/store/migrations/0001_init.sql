-- M0 init: a tiny meta table proves the embedded migration runner works and is
-- idempotent. The real spine (job_events ledger, jobs projection, leases) lands
-- in M1.
CREATE TABLE IF NOT EXISTS flowbee_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT INTO flowbee_meta (key, value)
VALUES ('schema_generation', 'm0')
ON CONFLICT (key) DO NOTHING;
