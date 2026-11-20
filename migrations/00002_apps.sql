-- +goose Up
CREATE TABLE projects (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    created_at INTEGER NOT NULL
) STRICT;

-- desired_status is what the admin asked for; runtime_status is what the
-- reconciler last observed in Docker.
CREATE TABLE apps (
    id                    INTEGER PRIMARY KEY,
    project_id            INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    name                  TEXT    NOT NULL COLLATE NOCASE,
    image                 TEXT    NOT NULL,
    port                  INTEGER NOT NULL DEFAULT 0,
    restart_policy        TEXT    NOT NULL DEFAULT 'unless-stopped',
    cpu_millis            INTEGER NOT NULL DEFAULT 0,
    memory_mb             INTEGER NOT NULL DEFAULT 0,
    desired_status        TEXT    NOT NULL DEFAULT 'running',
    runtime_status        TEXT    NOT NULL DEFAULT 'unknown',
    runtime_message       TEXT    NOT NULL DEFAULT '',
    current_deployment_id INTEGER,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    UNIQUE (project_id, name)
) STRICT;

CREATE TABLE deployments (
    id           INTEGER PRIMARY KEY,
    app_id       INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    image        TEXT    NOT NULL,
    image_id     TEXT    NOT NULL DEFAULT '',
    port         INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT 'queued',
    container_id TEXT    NOT NULL DEFAULT '',
    container_ip TEXT    NOT NULL DEFAULT '',
    error        TEXT    NOT NULL DEFAULT '',
    log          TEXT    NOT NULL DEFAULT '',
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    started_at   INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX deployments_app_id ON deployments(app_id, id);

-- Values are encrypted with the master key; only keys are readable.
CREATE TABLE env_vars (
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    value_enc  BLOB    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (app_id, key)
) STRICT;

CREATE TABLE domains (
    domain     TEXT    PRIMARY KEY COLLATE NOCASE,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX domains_app_id ON domains(app_id);

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL,
    ref_id      INTEGER NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'queued',
    attempts    INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    started_at  INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX jobs_status ON jobs(status, id);

-- +goose Down
DROP TABLE jobs;
DROP TABLE domains;
DROP TABLE env_vars;
DROP TABLE deployments;
DROP TABLE apps;
DROP TABLE projects;
