-- Lot 2 tables (spec §8.2). Sealed columns hold internal/secure values;
-- token hashes are SHA-256, email hashes HMAC-SHA256. Unix seconds, UTC.

CREATE TABLE tickets (
    id INTEGER PRIMARY KEY,
    ref TEXT UNIQUE,                 -- CPP-0042, NULL while draft
    token_hash BLOB NOT NULL UNIQUE,
    token BLOB NOT NULL,             -- sealed, to resend the original link
    form_key_hash BLOB NOT NULL UNIQUE,
    email_hash BLOB NOT NULL,
    category TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('draft', 'todo', 'in_progress', 'waiting', 'done')),
    assignee TEXT,
    version INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    submitted_at INTEGER,
    updated_at INTEGER NOT NULL,     -- last activity
    closed_at INTEGER,
    first_name BLOB NOT NULL,
    last_name BLOB NOT NULL,
    email BLOB NOT NULL,
    fields BLOB NOT NULL,            -- sealed JSON tickets.Fields
    description BLOB NOT NULL,
    -- spec §4.4: todo has no resolver, every other open ticket has one.
    CHECK ((status IN ('draft', 'todo') AND assignee IS NULL)
        OR (status IN ('in_progress', 'waiting') AND assignee IS NOT NULL)
        OR status = 'done'),
    CHECK ((status = 'done') = (closed_at IS NOT NULL)),
    CHECK ((status = 'draft') = (submitted_at IS NULL)),
    CHECK ((status = 'draft') = (ref IS NULL))
);
CREATE INDEX tickets_email_hash ON tickets (email_hash);
CREATE INDEX tickets_status ON tickets (status, submitted_at);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    ticket_id INTEGER NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    author_type TEXT NOT NULL CHECK (author_type IN ('member', 'admin')),
    author TEXT,
    internal INTEGER NOT NULL DEFAULT 0 CHECK (internal IN (0, 1)),
    created_at INTEGER NOT NULL,
    body BLOB NOT NULL,
    CHECK (internal = 0 OR author_type = 'admin'),
    CHECK ((author_type = 'admin') = (author IS NOT NULL))
);
CREATE INDEX messages_ticket ON messages (ticket_id);

CREATE TABLE attachments (
    id INTEGER PRIMARY KEY,
    ticket_id INTEGER NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    message_id INTEGER REFERENCES messages (id) ON DELETE CASCADE,
    mime TEXT NOT NULL CHECK (mime IN ('image/png', 'image/jpeg')),
    size INTEGER NOT NULL,
    object_key TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
CREATE INDEX attachments_ticket ON attachments (ticket_id);
CREATE INDEX attachments_message ON attachments (message_id);

CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    ticket_id INTEGER NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    actor TEXT NOT NULL,             -- username, 'member' or 'system'
    data TEXT NOT NULL DEFAULT '{}', -- JSON object of strings, never personal data
    created_at INTEGER NOT NULL
);
CREATE INDEX events_ticket ON events (ticket_id);

CREATE TABLE outbox (
    id INTEGER PRIMARY KEY,
    ticket_id INTEGER REFERENCES tickets (id) ON DELETE CASCADE,
    message_id INTEGER REFERENCES messages (id) ON DELETE CASCADE,
    event TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    give_up_at INTEGER NOT NULL,
    sent_at INTEGER,
    failed_at INTEGER,
    created_at INTEGER NOT NULL,
    recipient_hash BLOB NOT NULL,    -- HMAC of the normalized address, for erasure
    recipient BLOB NOT NULL,
    subject BLOB NOT NULL,
    body BLOB NOT NULL               -- sealed text part; HTML is rendered at send time
);
CREATE INDEX outbox_due ON outbox (status, next_attempt_at);
CREATE INDEX outbox_recipient ON outbox (recipient_hash);
CREATE INDEX outbox_ticket ON outbox (ticket_id);
CREATE INDEX outbox_message ON outbox (message_id);

CREATE TABLE stats_monthly (
    month TEXT NOT NULL,             -- 2026-10, Europe/Paris
    category TEXT NOT NULL,
    closed_count INTEGER NOT NULL,
    hours_to_close_total INTEGER NOT NULL,
    PRIMARY KEY (month, category)
);
