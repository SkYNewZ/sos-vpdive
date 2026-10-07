-- Lot 8 (spec §7.6 as amended): the club's activity calendar, pushed as JSON
-- by the external script. Events and participants are kept like the payment
-- lines: identifiers, dates and hashes in clear, everything else sealed.

-- imports gains the 'calendar' kind. SQLite cannot alter a CHECK constraint,
-- so the table is rebuilt with the two tables pointing at it, as in 0006:
-- each new table is filled before the old ones go, and renaming imports_new
-- rewrites the references of the new line tables. Ids are kept.
CREATE TABLE imports_new (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('members', 'payments', 'vpaydive', 'calendar')),
    exported_at INTEGER,
    period_from INTEGER,
    period_to INTEGER,
    imported_at INTEGER NOT NULL,
    imported_by TEXT NOT NULL,
    row_count INTEGER NOT NULL,
    skipped_count INTEGER NOT NULL,
    file_hash BLOB -- HMAC of the imported file; NULL before lot 7
);

INSERT INTO imports_new (id, kind, exported_at, period_from, period_to, imported_at, imported_by, row_count, skipped_count, file_hash)
SELECT id, kind, exported_at, period_from, period_to, imported_at, imported_by, row_count, skipped_count, file_hash
FROM imports;

CREATE TABLE payment_lines_new (
    id INTEGER PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports_new (id),
    name_hash BLOB NOT NULL,                                          -- HMAC of the normalized name
    ambiguous INTEGER NOT NULL DEFAULT 0 CHECK (ambiguous IN (0, 1)), -- set once, never cleared
    data BLOB NOT NULL                                                -- sealed JSON
);

INSERT INTO payment_lines_new (id, import_id, name_hash, ambiguous, data)
SELECT id, import_id, name_hash, ambiguous, data
FROM payment_lines;

CREATE TABLE online_payment_lines_new (
    id INTEGER PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports_new (id),
    name_hash BLOB NOT NULL,                                          -- HMAC of the normalized name
    ambiguous INTEGER NOT NULL DEFAULT 0 CHECK (ambiguous IN (0, 1)), -- set once, never cleared
    data BLOB NOT NULL                                                -- sealed JSON
);

INSERT INTO online_payment_lines_new (id, import_id, name_hash, ambiguous, data)
SELECT id, import_id, name_hash, ambiguous, data
FROM online_payment_lines;

DROP TABLE payment_lines;

DROP TABLE online_payment_lines;

DROP TABLE imports;

ALTER TABLE imports_new RENAME TO imports;

ALTER TABLE payment_lines_new RENAME TO payment_lines;

ALTER TABLE online_payment_lines_new RENAME TO online_payment_lines;

CREATE INDEX payment_lines_name_hash ON payment_lines (name_hash);

CREATE INDEX online_payment_lines_name_hash ON online_payment_lines (name_hash);

-- One row per event of the calendar, by the id the script sends. import_id is
-- the latest push that held it: a push deletes the events of its window it
-- did not touch.
CREATE TABLE calendar_events (
    id TEXT PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports (id),
    starts_at INTEGER NOT NULL, -- Unix
    ends_at INTEGER NOT NULL,   -- Unix; may precede starts_at, kept as pushed
    data BLOB NOT NULL          -- sealed JSON: the event without its participants
);

CREATE INDEX calendar_events_starts_at ON calendar_events (starts_at);

-- People an event knows: registered, pilots, payers. name_hash is set only
-- for a registered participant with both names: matching never runs on an
-- empty name or on the full name of an unregistered one.
CREATE TABLE calendar_participants (
    id INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES calendar_events (id) ON DELETE CASCADE,
    person_hash BLOB NOT NULL, -- HMAC of the VPDive id: the same person across events
    name_hash BLOB,            -- HMAC of the normalized name, as members.name_hash
    data BLOB NOT NULL         -- sealed JSON: the participant as pushed
);

CREATE INDEX calendar_participants_event ON calendar_participants (event_id);

CREATE INDEX calendar_participants_name_hash ON calendar_participants (name_hash);

CREATE INDEX calendar_participants_person_hash ON calendar_participants (person_hash);
