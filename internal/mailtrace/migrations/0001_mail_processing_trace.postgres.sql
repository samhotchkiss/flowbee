ALTER TABLE model_invocation
  ADD COLUMN IF NOT EXISTS message_id uuid NULL REFERENCES email_message(id);

ALTER TABLE model_invocation
  ADD COLUMN IF NOT EXISTS stage text NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_model_invocation_message_id
  ON model_invocation(message_id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_model_invocation_message_stage_created
  ON model_invocation(message_id, stage, created_at);

ALTER TABLE email_message_comprehension_heavy
  ADD COLUMN IF NOT EXISTS context_bundle_manifest jsonb NULL;
