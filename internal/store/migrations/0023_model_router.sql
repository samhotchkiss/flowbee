-- Centralized LLM model router: slot-backed bindings and per-attempt invocation
-- ledger. Flowbee itself still keeps provider calls out of the deterministic core;
-- this schema is for backend/router code that needs a data-backed model choice.

CREATE TABLE IF NOT EXISTS model_slot_binding (
    id TEXT PRIMARY KEY,
    slot_key TEXT NOT NULL CHECK (slot_key IN (
        'chat',
        'drafting-complex',
        'fact-check',
        'memory-extraction',
        'comprehension-light',
        'comprehension-heavy',
        'classification-light',
        'embeddings',
        'vision',
        'judge'
    )),
    tenant_id TEXT,
    model_id TEXT NOT NULL,
    provider_pins TEXT NOT NULL DEFAULT '{}',
    effort TEXT CHECK (effort IS NULL OR effort IN ('none', 'low', 'medium', 'high')),
    params TEXT NOT NULL DEFAULT '{}',
    privacy_tier_required TEXT NOT NULL CHECK (privacy_tier_required IN (
        'public',
        'internal',
        'confidential',
        'restricted'
    )),
    monthly_budget_usd NUMERIC(12,2),
    fallback_chain TEXT NOT NULL DEFAULT '[]',
    prompt_version_ref TEXT,
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    updated_by TEXT NOT NULL,
    benchmark_verdict_ref TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS model_slot_binding_one_active_global
    ON model_slot_binding(slot_key)
    WHERE tenant_id IS NULL AND active = 1;

CREATE UNIQUE INDEX IF NOT EXISTS model_slot_binding_one_active_tenant
    ON model_slot_binding(tenant_id, slot_key)
    WHERE tenant_id IS NOT NULL AND active = 1;

CREATE TABLE IF NOT EXISTS model_endpoint_policy (
    model_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    privacy_tier_supported TEXT NOT NULL CHECK (privacy_tier_supported IN (
        'public',
        'internal',
        'confidential',
        'restricted'
    )),
    data_retention_policy_ref TEXT NOT NULL,
    training_use_allowed INTEGER NOT NULL DEFAULT 0 CHECK (training_use_allowed IN (0, 1)),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (model_id, provider)
);

CREATE TABLE IF NOT EXISTS model_invocation (
    id TEXT PRIMARY KEY,
    slot_key TEXT NOT NULL,
    binding_id TEXT REFERENCES model_slot_binding(id),
    tenant_id TEXT,
    provider TEXT,
    model_id TEXT NOT NULL,
    status TEXT NOT NULL,
    attempt_index INTEGER NOT NULL DEFAULT 0,
    is_fallback INTEGER NOT NULL DEFAULT 0 CHECK (is_fallback IN (0, 1)),
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    total_tokens INTEGER,
    estimated_cost_usd NUMERIC(12,6),
    latency_ms INTEGER,
    ttft_ms INTEGER,
    error_code TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    CHECK (slot_key IN (
        'unknown',
        'chat',
        'drafting-complex',
        'fact-check',
        'memory-extraction',
        'comprehension-light',
        'comprehension-heavy',
        'classification-light',
        'embeddings',
        'vision',
        'judge'
    ))
);

CREATE INDEX IF NOT EXISTS model_invocation_slot_month
    ON model_invocation(slot_key, tenant_id, created_at);

CREATE INDEX IF NOT EXISTS model_invocation_binding_attempt
    ON model_invocation(binding_id, attempt_index);

CREATE TRIGGER IF NOT EXISTS model_invocation_slot_insert_guard
BEFORE INSERT ON model_invocation
WHEN NEW.slot_key IS NULL OR NEW.slot_key NOT IN (
    'chat',
    'drafting-complex',
    'fact-check',
    'memory-extraction',
    'comprehension-light',
    'comprehension-heavy',
    'classification-light',
    'embeddings',
    'vision',
    'judge'
)
BEGIN
    SELECT RAISE(ABORT, 'model_invocation.slot_key is required for new rows');
END;

CREATE TRIGGER IF NOT EXISTS model_invocation_slot_update_guard
BEFORE UPDATE OF slot_key ON model_invocation
WHEN NEW.slot_key IS NULL OR NEW.slot_key NOT IN (
    'chat',
    'drafting-complex',
    'fact-check',
    'memory-extraction',
    'comprehension-light',
    'comprehension-heavy',
    'classification-light',
    'embeddings',
    'vision',
    'judge'
)
BEGIN
    SELECT RAISE(ABORT, 'model_invocation.slot_key is required for new rows');
END;

CREATE TABLE IF NOT EXISTS model_budget_alert (
    scope_key TEXT NOT NULL,
    slot_key TEXT NOT NULL,
    budget_month TEXT NOT NULL,
    threshold_pct INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (scope_key, slot_key, budget_month, threshold_pct)
);

CREATE TABLE IF NOT EXISTS model_benchmark_verdict (
    id TEXT PRIMARY KEY,
    area TEXT NOT NULL,
    model_id TEXT NOT NULL,
    provider_pins TEXT NOT NULL,
    prompt_version_ref TEXT,
    status TEXT NOT NULL CHECK (status IN ('pass', 'fail')),
    evaluated_at TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at TEXT
);

CREATE TABLE IF NOT EXISTS model_incumbent_smoke (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES model_slot_binding(id),
    area TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pass', 'fail')),
    sample_count INTEGER NOT NULL DEFAULT 5,
    details TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT OR IGNORE INTO model_endpoint_policy
    (model_id, provider, privacy_tier_supported, data_retention_policy_ref, training_use_allowed)
VALUES
    ('claude-sonnet-5', 'anthropic', 'confidential', 'anthropic-cli-configured-account', 0),
    ('claude-opus-4-6', 'anthropic', 'confidential', 'anthropic-cli-configured-account', 0),
    ('sonnet', 'anthropic', 'confidential', 'anthropic-cli-alias', 0),
    ('opus', 'anthropic', 'confidential', 'anthropic-cli-alias', 0),
    ('codex', 'openai', 'confidential', 'codex-cli-configured-account', 0);

INSERT OR IGNORE INTO model_slot_binding
    (id, slot_key, tenant_id, model_id, provider_pins, effort, params, privacy_tier_required,
     monthly_budget_usd, fallback_chain, prompt_version_ref, active, updated_by, benchmark_verdict_ref)
VALUES
    ('00000000-0000-0000-0000-000000000102', 'drafting-complex', NULL, 'sonnet',
     '{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}',
     'none', '{}', 'internal', NULL, '[]', NULL, 1, 'migration:0023_model_router', NULL),
    ('00000000-0000-0000-0000-000000000110', 'judge', NULL, 'opus',
     '{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}',
     'none', '{}', 'internal', NULL, '[]', NULL, 1, 'migration:0023_model_router', NULL);
