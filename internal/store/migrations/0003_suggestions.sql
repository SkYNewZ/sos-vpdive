-- Lot 3 (spec §3.2, §5, §8.2): screen 2 and the model's choice.

ALTER TABLE tickets ADD COLUMN draft_token_hash BLOB; -- SHA-256 of the screen 2 token
ALTER TABLE tickets ADD COLUMN kb_ids TEXT;           -- JSON array of fiche ids; NULL when the model did not answer
ALTER TABLE tickets ADD COLUMN summary BLOB;          -- sealed; NULL without a model answer
-- SQLite cannot add a UNIQUE column: the index carries it, NULLs allowed.
CREATE UNIQUE INDEX tickets_draft_token ON tickets (draft_token_hash);

-- « Ça règle mon problème » (spec §3.2): no personal data.
CREATE TABLE deflections (
    id INTEGER PRIMARY KEY,
    category TEXT NOT NULL,
    kb_ids TEXT NOT NULL,                             -- JSON array of the fiches shown
    created_at INTEGER NOT NULL
);

-- Reads for the committee and the member go through this view: a draft is
-- never shown (spec §3.2).
CREATE VIEW submitted_tickets AS SELECT * FROM tickets WHERE status != 'draft';
