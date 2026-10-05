-- Lot 1 tables (spec §8.2). Personal data columns hold values sealed by
-- internal/secure; hashes are HMAC-SHA256 (email, name) or SHA-256 (tokens).
-- Timestamps are Unix seconds, UTC.

CREATE TABLE members (
    id INTEGER PRIMARY KEY,
    email_hash BLOB NOT NULL UNIQUE,
    name_hash BLOB NOT NULL,
    first_name BLOB NOT NULL,
    last_name BLOB NOT NULL,
    email BLOB NOT NULL,
    seasons BLOB,           -- NULL when the export has no "Année(s)" column
    licence_expires BLOB    -- NULL when empty or unreadable
);

CREATE INDEX members_name_hash ON members (name_hash);

CREATE TABLE imports (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('members', 'payments')),
    exported_at INTEGER,
    period_from INTEGER,
    period_to INTEGER,
    imported_at INTEGER NOT NULL,
    imported_by TEXT NOT NULL,
    row_count INTEGER NOT NULL,
    skipped_count INTEGER NOT NULL
);

-- credential_hash: SHA-256 of the account's password hash at login. A session
-- is valid only while it matches the accounts file (design §2).
CREATE TABLE sessions (
    token_hash BLOB PRIMARY KEY,
    username TEXT NOT NULL,
    credential_hash BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_username ON sessions (username);

CREATE TABLE counters (
    key TEXT PRIMARY KEY,
    window_start INTEGER NOT NULL,
    count INTEGER NOT NULL
);
