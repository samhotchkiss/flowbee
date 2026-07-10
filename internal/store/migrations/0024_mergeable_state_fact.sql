-- 0024: persist GitHub's computed PR mergeability as a Domain-B fact. The merge
-- gate treats explicit UNKNOWN as not mergeable, preventing transition to
-- merge_handoff/merging before GitHub has finished computing mergeability.
ALTER TABLE domain_b_facts ADD COLUMN mergeable_state TEXT NOT NULL DEFAULT '';
