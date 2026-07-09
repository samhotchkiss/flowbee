-- 0023: targeted adopted PR review diffs can be authoritatively empty. patch_diff=''
-- alone was already the legacy "missing diff" value, so record the explicit empty
-- state separately to prevent review harnesses from treating empty PRs as missing
-- artifacts.
ALTER TABLE jobs ADD COLUMN diff_empty INTEGER NOT NULL DEFAULT 0;
