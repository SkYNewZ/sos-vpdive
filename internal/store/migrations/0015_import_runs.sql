-- Import runs (design 2026-10-09): every run of an import, succeeded or
-- not, so the imports page shows the script's runs and their outcome. A
-- manual import or an accepted push is 'imported'; a push with the bytes of
-- the latest import is 'unchanged'; a file the service refused is 'refused'
-- with its code and the message the committee would read; a run the script
-- could not complete is 'failed' with the class it reports. Rows are kept
-- 90 days.
CREATE TABLE import_runs (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('members', 'payments', 'vpaydive', 'calendar', 'carnets')),
    at INTEGER NOT NULL,
    by TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('imported', 'unchanged', 'refused', 'failed')),
    code TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    row_count INTEGER NOT NULL DEFAULT 0,
    skipped_count INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX import_runs_kind_at ON import_runs (kind, at);
