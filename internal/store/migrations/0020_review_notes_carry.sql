-- 0020: carry the reviewer's findings forward to the rebuild (compounding memory, §F read).
-- When a code-review bounces (changes_requested), the reviewer's findings are now persisted
-- on the job so the next build's lease context can surface them — the agent fixes what was
-- flagged instead of rebuilding blind and re-submitting a patch that already failed review.
-- A projection field folded from the bounce event's payload (projection == Fold(events)).
ALTER TABLE jobs ADD COLUMN last_review_notes TEXT NOT NULL DEFAULT '';
