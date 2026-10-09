-- Account photos (2026-10-09): the name of the account's sealed photo file
-- under DATA_DIR/avatars, NULL for the drawn avatar.
ALTER TABLE accounts ADD COLUMN avatar_file TEXT;
