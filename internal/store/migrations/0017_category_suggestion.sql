-- Committee-only category advice; the inferred category is sealed like the summary.
ALTER TABLE tickets ADD COLUMN suggested_category BLOB;
-- Resolution survives late model answers and reopening a closed ticket.
ALTER TABLE tickets ADD COLUMN category_suggestion_resolved INTEGER NOT NULL DEFAULT 0
    CHECK (category_suggestion_resolved IN (0, 1));
