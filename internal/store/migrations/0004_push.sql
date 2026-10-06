-- Lot 4 (spec §6, §8.2, §9.6): committee alerts on Pushover and Web Push.

-- Alerts share the outbox with mails: one attempt each, no recipient.
ALTER TABLE outbox ADD COLUMN channel TEXT NOT NULL DEFAULT 'email'
    CHECK (channel IN ('email', 'pushover', 'webpush'));

CREATE TABLE push_subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT, -- ids reach logs: never reused
    session_token_hash BLOB NOT NULL REFERENCES sessions (token_hash) ON DELETE CASCADE,
    username TEXT NOT NULL,
    endpoint_hash BLOB NOT NULL UNIQUE,   -- SHA-256 of the endpoint: one row per browser
    created_at INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,        -- last successful push, created_at until then
    endpoint BLOB NOT NULL,               -- sealed
    keys BLOB NOT NULL                    -- sealed JSON {"p256dh": "...", "auth": "..."}
);

CREATE INDEX push_subscriptions_session ON push_subscriptions (session_token_hash);
