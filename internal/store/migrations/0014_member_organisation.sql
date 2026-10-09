-- Active members (2026-10-09): the VPDive groups of the members export's
-- "Organisation" column, sealed; NULL when the export had no such column.
ALTER TABLE members ADD COLUMN organisation BLOB;
