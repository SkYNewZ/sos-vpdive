-- Carnets (design 2026-10-09): the carnet carts the script pushes from
-- VPDive's payments page, stored like the payment lines: the holder's name
-- hash in clear, the cart sealed.

-- imports gains the 'carnets' kind. SQLite cannot alter a CHECK constraint,
-- so the table is rebuilt with every table pointing at it, as in 0008: the
-- payment and Mollie lines and the calendar events, with the participants and
-- unregistrations that point at the events. Each new table is filled before
-- the old ones go; renaming imports_new and calendar_events_new rewrites the
-- references of the new tables. Ids are kept.
CREATE TABLE imports_new (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('members', 'payments', 'vpaydive', 'calendar', 'carnets')),
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

CREATE TABLE calendar_events_new (
    id TEXT PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports_new (id),
    starts_at INTEGER NOT NULL, -- Unix
    ends_at INTEGER NOT NULL,   -- Unix; may precede starts_at, kept as pushed
    data BLOB NOT NULL          -- sealed JSON: the event without its participants
);

INSERT INTO calendar_events_new (id, import_id, starts_at, ends_at, data)
SELECT id, import_id, starts_at, ends_at, data
FROM calendar_events;

CREATE TABLE calendar_participants_new (
    id INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES calendar_events_new (id) ON DELETE CASCADE,
    person_hash BLOB NOT NULL, -- HMAC of the VPDive id: the same person across events
    name_hash BLOB,            -- HMAC of the normalized name, as members.name_hash
    data BLOB NOT NULL         -- sealed JSON: the participant as pushed
);

INSERT INTO calendar_participants_new (id, event_id, person_hash, name_hash, data)
SELECT id, event_id, person_hash, name_hash, data
FROM calendar_participants;

CREATE TABLE calendar_unregistrations_new (
    id INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES calendar_events_new (id) ON DELETE CASCADE,
    name_hash BLOB NOT NULL, -- HMAC of the normalized name, as members.name_hash
    data BLOB NOT NULL       -- sealed JSON: the unregistration as pushed
);

INSERT INTO calendar_unregistrations_new (id, event_id, name_hash, data)
SELECT id, event_id, name_hash, data
FROM calendar_unregistrations;

DROP TABLE calendar_unregistrations;

DROP TABLE calendar_participants;

DROP TABLE calendar_events;

DROP TABLE online_payment_lines;

DROP TABLE payment_lines;

DROP TABLE imports;

ALTER TABLE imports_new RENAME TO imports;

ALTER TABLE payment_lines_new RENAME TO payment_lines;

ALTER TABLE online_payment_lines_new RENAME TO online_payment_lines;

ALTER TABLE calendar_events_new RENAME TO calendar_events;

ALTER TABLE calendar_participants_new RENAME TO calendar_participants;

ALTER TABLE calendar_unregistrations_new RENAME TO calendar_unregistrations;

CREATE INDEX payment_lines_name_hash ON payment_lines (name_hash);

CREATE INDEX online_payment_lines_name_hash ON online_payment_lines (name_hash);

CREATE INDEX calendar_events_starts_at ON calendar_events (starts_at);

CREATE INDEX calendar_participants_event ON calendar_participants (event_id);

CREATE INDEX calendar_participants_name_hash ON calendar_participants (name_hash);

CREATE INDEX calendar_participants_person_hash ON calendar_participants (person_hash);

CREATE INDEX calendar_unregistrations_event ON calendar_unregistrations (event_id);

CREATE INDEX calendar_unregistrations_name_hash ON calendar_unregistrations (name_hash);

-- One row per card of the latest push, replaced whole by each push.
CREATE TABLE carnets (
    id INTEGER PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports (id),
    name_hash BLOB NOT NULL,                                          -- HMAC of the holder's normalized name, as members.name_hash
    ambiguous INTEGER NOT NULL DEFAULT 0 CHECK (ambiguous IN (0, 1)), -- set once, never cleared
    data BLOB NOT NULL                                                -- sealed JSON: the cart as pushed
);

CREATE INDEX carnets_name_hash ON carnets (name_hash);
