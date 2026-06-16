-- M4: enforced anti-affinity at lease time (I-10, §5.5 / §6.3.1).
--
-- The §6.3.1 claim predicate excludes, for a code_reviewer lease, the bound
-- eng_worker's identity AND model_family. But the LIVE bound_identity /
-- bound_model_family columns are cleared the moment the build result lands
-- (review_pending), so the sibling's builder identity must be persisted
-- DURABLY for the review claim to read. These two columns are written ONCE,
-- when the build result transitions the job to review_pending, and are never
-- cleared — they are the anti-affinity input the review claim reasons over.
--
-- (eng_worker_job / code_reviewer_job sibling pointers already exist, 0002.)
ALTER TABLE jobs ADD COLUMN builder_identity     TEXT;   -- identity that built the patch (durable)
ALTER TABLE jobs ADD COLUMN builder_model_family TEXT;   -- model_family that built the patch (durable)
