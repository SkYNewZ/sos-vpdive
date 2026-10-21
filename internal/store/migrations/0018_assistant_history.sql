-- Private conversation archives. Content, titles and provider context are sealed.
CREATE TABLE assistant_conversations (
    id TEXT PRIMARY KEY,
    account TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    ticket_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    seen_at INTEGER NOT NULL,
    question_count INTEGER NOT NULL DEFAULT 0,
    history_bytes INTEGER NOT NULL DEFAULT 0,
    title BLOB NOT NULL,
    transcript BLOB NOT NULL,
    context BLOB NOT NULL,
    revision INTEGER NOT NULL DEFAULT 0,
    title_attempted INTEGER NOT NULL DEFAULT 0 CHECK (title_attempted IN (0, 1))
);
CREATE INDEX assistant_conversations_account_seen ON assistant_conversations(account, seen_at DESC, id DESC);
CREATE INDEX assistant_conversations_created ON assistant_conversations(created_at);
