-- Committee assistant usage (design 2026-10-08): one row per question,
-- refused ones included. No text, no member, no request: who, when, what it
-- cost and how it ended. Purged after 12 months.
CREATE TABLE assistant_usage (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account TEXT NOT NULL,
    at INTEGER NOT NULL,
    origin TEXT NOT NULL CHECK (origin IN ('page', 'demande')),
    model TEXT NOT NULL,
    thinking INTEGER NOT NULL,
    calls INTEGER NOT NULL,
    tools TEXT NOT NULL,          -- JSON {"find_member": 1, ...}
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    cost_micro_usd INTEGER,       -- NULL without prices
    first_token_ms INTEGER,       -- NULL without text
    duration_ms INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('ok', 'timeout', 'http_error', 'invalid_output', 'limit', 'canceled', 'internal'))
);

CREATE INDEX assistant_usage_at ON assistant_usage (at);
