-- M9: Content-integrity gate (DESIGN §9.2, I-11) — the Branch-B safety boundary.
-- The eng_worker's returned diff is UNTRUSTED DATA: before it is auto-merge-eligible
-- (the §5.4 predicate, conditions 2–4) it must clear three deterministic, non-LLM
-- gates: a path denylist (.github/workflows/**, lockfiles+lifecycle, Dockerfiles,
-- secrets, Flowbee's own source + the denylist itself); a declared-vs-actual
-- blast-radius check; and static checks (applies-clean@base, parse, secret-scan,
-- binary allowlist, size bounds). The gate is computed by the runtime over these
-- stored columns and threaded into the pure engine (EngineState.Content).
--
-- SQLite translation per the project overrides: TEXT columns, no TIMESTAMPTZ. The
-- patch diff is stored verbatim at build-result time (the untrusted bytes the gate
-- judges); the declared blast-radius is the worker's commitment (verified against
-- the actual diff); content_result caches the last computed deterministic Result
-- (audit + the §5.4 predicate read).

ALTER TABLE jobs ADD COLUMN patch_diff            TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN declared_blast_radius TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN content_result        TEXT NOT NULL DEFAULT '';
