-- Lot 5 (spec §7.3, §8.2): lines of the VPDive payments export. Names are
-- dropped after hashing; the kept columns are one sealed JSON value. Lines are
-- attributed at display time, through the members list.

CREATE TABLE payment_lines (
    id INTEGER PRIMARY KEY,
    import_id INTEGER NOT NULL REFERENCES imports (id),
    name_hash BLOB NOT NULL,                                       -- HMAC of the normalized name
    ambiguous INTEGER NOT NULL DEFAULT 0 CHECK (ambiguous IN (0, 1)), -- set once, never cleared
    data BLOB NOT NULL                                             -- sealed JSON
);

CREATE INDEX payment_lines_name_hash ON payment_lines (name_hash);
