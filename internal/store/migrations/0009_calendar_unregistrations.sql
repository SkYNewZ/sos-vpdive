-- Lot 8 part 3: the people who left an event, as the script pushes them:
-- names, when, by whom. Keyed by the name hash only (they carry no VPDive
-- id), everything else sealed. They go with their event.
CREATE TABLE calendar_unregistrations (
    id INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES calendar_events (id) ON DELETE CASCADE,
    name_hash BLOB NOT NULL, -- HMAC of the normalized name, as members.name_hash
    data BLOB NOT NULL       -- sealed JSON: the unregistration as pushed
);

CREATE INDEX calendar_unregistrations_event ON calendar_unregistrations (event_id);

CREATE INDEX calendar_unregistrations_name_hash ON calendar_unregistrations (name_hash);

-- The last calendar push may have been recorded by a version that dropped
-- this field: forget its hash, so that the same body is imported again, not
-- answered unchanged.
UPDATE imports SET file_hash = NULL WHERE kind = 'calendar';
