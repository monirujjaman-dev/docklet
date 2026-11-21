-- +goose Up
-- Workers are apps without a public route; command overrides the image CMD.
ALTER TABLE apps ADD COLUMN kind TEXT NOT NULL DEFAULT 'web'; -- web | worker
ALTER TABLE apps ADD COLUMN command TEXT NOT NULL DEFAULT '';

-- Cron jobs run a one-off container from their app's current image.
CREATE TABLE cron_jobs (
    id          INTEGER PRIMARY KEY,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL COLLATE NOCASE,
    schedule    TEXT    NOT NULL,
    timezone    TEXT    NOT NULL DEFAULT 'UTC',
    command     TEXT    NOT NULL,
    timeout_sec INTEGER NOT NULL DEFAULT 3600,
    concurrency TEXT    NOT NULL DEFAULT 'skip', -- skip | allow
    enabled     INTEGER NOT NULL DEFAULT 1,
    next_run_at INTEGER NOT NULL DEFAULT 0,      -- unix seconds; 0 = not scheduled
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (app_id, name)
) STRICT;

CREATE INDEX cron_jobs_due ON cron_jobs (enabled, next_run_at);

CREATE TABLE cron_runs (
    id           INTEGER PRIMARY KEY,
    job_id       INTEGER NOT NULL REFERENCES cron_jobs(id) ON DELETE CASCADE,
    trigger      TEXT    NOT NULL,                -- schedule | manual
    status       TEXT    NOT NULL,                -- running | succeeded | failed | timeout | skipped
    exit_code    INTEGER,
    container_id TEXT    NOT NULL DEFAULT '',
    error        TEXT    NOT NULL DEFAULT '',
    logs         TEXT    NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER,
    duration_ms  INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX cron_runs_job ON cron_runs (job_id, id);

-- +goose Down
DROP TABLE cron_runs;
DROP TABLE cron_jobs;
ALTER TABLE apps DROP COLUMN command;
ALTER TABLE apps DROP COLUMN kind;
