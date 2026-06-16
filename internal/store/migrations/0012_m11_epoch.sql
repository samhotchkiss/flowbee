-- M11: Epoch-namespaced side-effects + compensation (DESIGN §3.5/§6.5, I-12) — the
-- structural realization of T2 (ack != execution) that enables unattended merge.
--
-- Fencing (lease_epoch) gates calls back INTO Flowbee; git/CI/GitHub never see the
-- token. So every externally-visible action is made idempotent against re-dispatch
-- structurally: epoch-namespaced refs (promoted only post-validation), (job,epoch)-
-- scoped CI (a zombie's checks fired from a stale epoch's ref can never satisfy a
-- live job's gate), and explicit compensation on revocation (drop the dead ref,
-- cancel the dead epoch's CI, draft-back any PR opened for the dead attempt, bump
-- the epoch so the reconnecting worker is fenced 409).
--
-- SQLite translation per the project overrides: TEXT/INTEGER columns, no TIMESTAMPTZ.

-- ── (job, epoch)-scoped CI (§6.5.2) ──
-- CI status is keyed (job_id, epoch). A worker push to refs/flowbee/<job>/epoch-<n>
-- triggers CI recorded HERE against that epoch. The live code-review gate consumes
-- ONLY the live epoch's row — so a zombie that pushed to a STALE epoch and turned its
-- CI green cannot satisfy the live job's gate (the live epoch has no green row).
CREATE TABLE epoch_ci (
    job_id     TEXT    NOT NULL,
    epoch      INTEGER NOT NULL,
    head_sha   TEXT    NOT NULL DEFAULT '',
    ci_state   TEXT    NOT NULL DEFAULT 'pending', -- pending | success | failure | cancelled
    promoted   INTEGER NOT NULL DEFAULT 0,          -- 1 once Flowbee fast-forwarded this epoch's ref
    updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (job_id, epoch)
);

-- ── compensation audit (§6.5.4) ──
-- Every revocation/expiry/supersede fires compensate(job, dead_epoch). This records
-- the intent + the actions taken (ref dropped, CI cancelled, PR drafted-back) so the
-- audit log is complete and a crash mid-compensate replays cleanly. Keyed
-- (job_id, dead_epoch) so re-running compensation for the same dead epoch is a no-op.
CREATE TABLE compensations (
    job_id        TEXT    NOT NULL,
    dead_epoch    INTEGER NOT NULL,
    ref_dropped   INTEGER NOT NULL DEFAULT 0,
    ci_cancelled  INTEGER NOT NULL DEFAULT 0,
    pr_drafted    INTEGER NOT NULL DEFAULT 0,
    reason        TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (job_id, dead_epoch)
);

-- the epoch whose ref was last PROMOTED onto the real branch (the live build epoch).
-- A result from a stale epoch (build_epoch already advanced) is never promoted.
ALTER TABLE jobs ADD COLUMN build_epoch INTEGER NOT NULL DEFAULT 0;
-- the merge commit Flowbee observed reconciled-true after an unattended self_merge
-- (the terminal Domain-B fact, recorded for provenance on the done transition).
ALTER TABLE jobs ADD COLUMN merge_provenance TEXT NOT NULL DEFAULT '';
