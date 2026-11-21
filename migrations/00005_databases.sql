-- +goose Up
-- Managed databases. Credentials are encrypted with the master key.
CREATE TABLE databases (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    name            TEXT    NOT NULL COLLATE NOCASE,
    engine          TEXT    NOT NULL,            -- postgres | mysql | mariadb | redis
    version         TEXT    NOT NULL,            -- image tag
    db_name         TEXT    NOT NULL DEFAULT '',
    username        TEXT    NOT NULL DEFAULT '',
    password_enc    BLOB    NOT NULL,
    root_password_enc BLOB,
    memory_mb       INTEGER NOT NULL DEFAULT 0,
    public_port     INTEGER NOT NULL DEFAULT 0,  -- 0 = not published
    container_id    TEXT    NOT NULL DEFAULT '',
    desired_status  TEXT    NOT NULL DEFAULT 'running',
    runtime_status  TEXT    NOT NULL DEFAULT 'creating',
    runtime_message TEXT    NOT NULL DEFAULT '',
    backup_schedule TEXT    NOT NULL DEFAULT 'daily', -- off | hourly | daily | weekly
    backup_keep     INTEGER NOT NULL DEFAULT 7,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (project_id, name)
) STRICT;

CREATE TABLE backups (
    id          INTEGER PRIMARY KEY,
    database_id INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    trigger     TEXT    NOT NULL,                 -- manual | schedule | pre-restore
    status      TEXT    NOT NULL DEFAULT 'running',
    file        TEXT    NOT NULL DEFAULT '',      -- relative to the backups directory
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX backups_database ON backups(database_id, id);

-- A link writes the connection URL into an app's environment.
CREATE TABLE database_links (
    database_id INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    env_key     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (database_id, app_id)
) STRICT;

-- +goose Down
DROP TABLE database_links;
DROP TABLE backups;
DROP TABLE databases;
