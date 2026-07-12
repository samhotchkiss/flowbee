-- F6 preemptive ceiling: per-account TOKEN-BUDGET accumulation so usage_pct is a
-- REAL rising percentage (not the old binary 0/100), letting dispatch roll over to
-- a less-used account BEFORE the busy one hits the hard 429.
--
-- codex exposes no live usage % — only per-run token counts. So a box reports the
-- INCREMENTAL tokens each run consumed and the server accumulates them here, over
-- the account's reset window, deriving usage_pct = window_tokens / budget_tokens.
-- Accumulation lives on the ACCOUNT row (not the box) so every box on the same
-- login folds into ONE shared bucket (per-login sharing, §C). The ceiling gate
-- (usage_pct >= ceiling_pct) needs NO change — it now trips preemptively at 90%.
--
-- window_started_at marks the current reset window's start; when now - that exceeds
-- the configured window length the store ZEROES window_tokens (fresh window) so the
-- account un-gates and sharing resumes. budget_tokens 0 = use the server default;
-- a per-account override lets a bigger-quota login carry more before it gates.

ALTER TABLE worker_accounts ADD COLUMN window_tokens     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE worker_accounts ADD COLUMN budget_tokens     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE worker_accounts ADD COLUMN window_started_at TEXT;
