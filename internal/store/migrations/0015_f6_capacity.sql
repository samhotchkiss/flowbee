-- F6: worker capacity — per-model slots, named accounts, usage, ceilings, weights.
--
-- A BOX (worker) no longer advertises a single max_concurrent_leases. Instead it
-- advertises concurrency PER MODEL (claude:3, codex:3) and a per-box distribution
-- WEIGHT (user-controlled bias). Each model has NAMED ACCOUNTS (per-model creds,
-- shared across boxes on the same login) with a ceiling_pct and an ordered
-- PREFERENCE so dispatch rolls over from a near-ceiling account to its fallback.
-- USAGE is tracked PER ACCOUNT (not per box) and reported via POST
-- /v1/workers/usage (~15 min, immediate on 429); a 429-spike pins usage to 100%
-- so dispatch is gated off that account until it cools down.
--
-- The single open conn (MaxOpenConns=1) serializes every read+write, so the
-- ceiling-gated account selection done in the lease claim is atomic.

-- ── per-box, per-model concurrency advertisement (§C "box = multi-model") ──
-- One row per (worker, model_family): the box advertises how many concurrent
-- leases it will run for that model. Replaces workers.max_concurrent_leases (now
-- vestigial — kept for back-compat but no longer the slot source). The default
-- distribution weight lives on the worker row.
CREATE TABLE worker_model_slots (
    worker_id    TEXT NOT NULL,
    model_family TEXT NOT NULL,                 -- claude | codex | …
    max_slots    INTEGER NOT NULL DEFAULT 0,    -- concurrency the box runs for this model
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (worker_id, model_family)
);

-- per-box distribution weight (user-controlled bias; default 1 = even spread).
ALTER TABLE workers ADD COLUMN distribution_weight INTEGER NOT NULL DEFAULT 1;

-- ── named accounts: per-model credentials with a ceiling + ordered preference ──
-- An account is a named login for a model (e.g. claude-primary, claude-fallback).
-- Usage is tracked PER ACCOUNT and SHARED across every box on the same login, so
-- the account row is the canonical usage bucket (not the worker). preference_rank
-- orders the rollover chain (lower = preferred); a dispatch picks the lowest-rank
-- account that is BELOW its ceiling, rolling over to the next when the preferred
-- account is at/over ceiling.
CREATE TABLE worker_accounts (
    account_id      TEXT PRIMARY KEY,            -- stable account name (claude-primary)
    model_family    TEXT NOT NULL,               -- which model this login serves
    ceiling_pct     INTEGER NOT NULL DEFAULT 90, -- gate: ≥ this usage% -> don't dispatch here
    preference_rank INTEGER NOT NULL DEFAULT 0,  -- rollover order (lower = preferred)
    usage_pct       INTEGER NOT NULL DEFAULT 0,  -- last-reported usage% (per-account)
    rate_limited    INTEGER NOT NULL DEFAULT 0,  -- 1 = a 429 pinned this account (cool-down)
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    reported_at     TEXT                          -- last usage report instant (RFC3339)
);

CREATE INDEX idx_worker_accounts_model_rank
    ON worker_accounts (model_family, preference_rank ASC, account_id ASC);

-- bind a claimed lease to the account it dispatched against, so per-account slot
-- accounting and usage attribution are exact. Empty for legacy/pre-F6 jobs.
ALTER TABLE jobs ADD COLUMN bound_account TEXT NOT NULL DEFAULT '';
