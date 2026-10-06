-- Lot 7 (spec §7.5 to §7.7, §8.2): Mollie collections from the VPayDive export,
-- the file hash that lets a pushed import skip an unchanged file, and the
-- lines a resolver checked and masked.

-- imports gains the 'vpaydive' kind and file_hash. SQLite cannot alter a
-- CHECK constraint, so the table is rebuilt. payment_lines points at it and is
-- rebuilt with it: each new table is filled before the old ones go, and
-- renaming imports_new rewrites the reference of payment_lines_new, so no
-- foreign key is ever left dangling. Ids are kept.
CREATE TABLE imports_new (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('members', 'payments', 'vpaydive')),
    exported_at INTEGER,
    period_from INTEGER,
    period_to INTEGER,
    imported_at INTEGER NOT NULL,
    imported_by TEXT NOT NULL,
    row_count INTEGER NOT NULL,
    skipped_count INTEGER NOT NULL,
    file_hash BLOB -- HMAC of the imported file; NULL before lot 7
);

INSERT INTO imports_new (id, kind, exported_at, period_from, period_to, imported_at, imported_by, row_count, skipped_count)
SELECT id, kind, exported_at, period_from, period_to, imported_at, imported_by, row_count, skipped_count
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

DROP TABLE payment_lines;

DROP TABLE imports;

ALTER TABLE imports_new RENAME TO imports;

ALTER TABLE payment_lines_new RENAME TO payment_lines;

CREATE INDEX payment_lines_name_hash ON payment_lines (name_hash);

-- Lines of the VPayDive export, stored like payment_lines: names dropped after
-- hashing, kept columns in one sealed JSON value, attribution at display time.
CREATE TABLE online_payment_lines (
    id INTEGER PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports (id),
    name_hash BLOB NOT NULL,                                          -- HMAC of the normalized name
    ambiguous INTEGER NOT NULL DEFAULT 0 CHECK (ambiguous IN (0, 1)), -- set once, never cleared
    data BLOB NOT NULL                                                -- sealed JSON
);

CREATE INDEX online_payment_lines_name_hash ON online_payment_lines (name_hash);

-- Lines to check that a resolver masked (spec §7.7). The fingerprint is an
-- HMAC of the line: it does not lead back to the person.
CREATE TABLE dismissed_checks (
    fingerprint TEXT PRIMARY KEY, -- hex
    dismissed_by TEXT NOT NULL,
    dismissed_at INTEGER NOT NULL
);
