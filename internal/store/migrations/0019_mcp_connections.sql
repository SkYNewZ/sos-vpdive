-- One grant of one committee account to one MCP client (Claude, ChatGPT).
-- Tokens are stored hashed. credential_hash binds the grant to the account's
-- password, as sessions do: a change or a reset ends it. scope holds what
-- the account granted, space-separated.
CREATE TABLE mcp_connections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    credential_hash BLOB NOT NULL,
    client_id TEXT NOT NULL,              -- the URL of the client id document
    client_name TEXT NOT NULL,            -- its client_name, as the page shows it
    scope TEXT NOT NULL,
    access_hash BLOB NOT NULL UNIQUE,
    access_expires_at INTEGER NOT NULL,
    refresh_hash BLOB NOT NULL UNIQUE,
    previous_refresh_hash BLOB UNIQUE,    -- the token the last refresh replaced: a late replay revokes
    created_at INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL           -- of the refresh token: 30 days after its last use
);
CREATE INDEX mcp_connections_username ON mcp_connections (username);
