-- Committee accounts move from the accounts file to the database (spec §4.1
-- and §8.2 as amended): the owner creates them on the committee site.
-- name and pushover_user_key are sealed (spec §8.4).
CREATE TABLE accounts (
    username TEXT PRIMARY KEY,
    name BLOB NOT NULL,
    role TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    must_change_password INTEGER NOT NULL CHECK (must_change_password IN (0, 1)),
    pushover_user_key BLOB,
    created_at INTEGER NOT NULL
);
