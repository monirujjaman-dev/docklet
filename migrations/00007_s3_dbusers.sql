-- +goose Up
-- S3-compatible buckets used as backup destinations.
CREATE TABLE s3_destinations (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    endpoint        TEXT    NOT NULL,
    region          TEXT    NOT NULL DEFAULT '',
    bucket          TEXT    NOT NULL,
    prefix          TEXT    NOT NULL DEFAULT '',
    access_key      TEXT    NOT NULL,
    secret_enc      BLOB    NOT NULL,
    path_style      INTEGER NOT NULL DEFAULT 0,
    last_test_at    INTEGER NOT NULL DEFAULT 0,
    last_test_error TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

-- Where a database's backups are uploaded (NULL = local only).
ALTER TABLE databases ADD COLUMN backup_destination_id INTEGER REFERENCES s3_destinations(id);

-- Remote copy of a backup.
ALTER TABLE backups ADD COLUMN destination_id INTEGER;               -- where it was uploaded
ALTER TABLE backups ADD COLUMN remote_key TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN remote_status TEXT NOT NULL DEFAULT ''; -- '' | uploading | uploaded | failed
ALTER TABLE backups ADD COLUMN remote_error TEXT NOT NULL DEFAULT '';

-- Extra users of a managed database. Passwords are encrypted.
CREATE TABLE database_users (
    id           INTEGER PRIMARY KEY,
    database_id  INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    username     TEXT    NOT NULL,
    password_enc BLOB    NOT NULL,
    role         TEXT    NOT NULL, -- readonly | readwrite | admin
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (database_id, username)
) STRICT;

-- +goose Down
DROP TABLE database_users;
ALTER TABLE backups DROP COLUMN remote_error;
ALTER TABLE backups DROP COLUMN remote_status;
ALTER TABLE backups DROP COLUMN remote_key;
ALTER TABLE backups DROP COLUMN destination_id;
ALTER TABLE databases DROP COLUMN backup_destination_id;
DROP TABLE s3_destinations;
