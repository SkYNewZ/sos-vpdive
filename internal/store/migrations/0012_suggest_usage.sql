-- Suggestion calls (2026-10-09), for the usage dashboard: one row per call
-- of the form's model, failed ones included. No text, no request, no member:
-- when, which model, what it cost and how it ended. Purged after 12 months.
CREATE TABLE suggest_usage (
    id INTEGER PRIMARY KEY,
    at INTEGER NOT NULL,
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cost_micro_usd INTEGER,       -- NULL without prices
    duration_ms INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('ok', 'timeout', 'http_error', 'invalid_output', 'canceled'))
);

CREATE INDEX suggest_usage_at ON suggest_usage (at);
