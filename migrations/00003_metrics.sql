-- +goose Up
-- One-minute averages. scope is "host" or "app:<id>". Rates are per second.
CREATE TABLE metrics (
    scope      TEXT    NOT NULL,
    ts         INTEGER NOT NULL, -- unix seconds, start of the minute
    cpu        REAL    NOT NULL,
    mem_used   INTEGER NOT NULL,
    mem_limit  INTEGER NOT NULL,
    disk_used  INTEGER NOT NULL DEFAULT 0,
    disk_total INTEGER NOT NULL DEFAULT 0,
    net_rx     REAL    NOT NULL,
    net_tx     REAL    NOT NULL,
    blk_read   REAL    NOT NULL,
    blk_write  REAL    NOT NULL,
    PRIMARY KEY (scope, ts)
) STRICT, WITHOUT ROWID;
CREATE INDEX metrics_ts ON metrics(ts);

-- +goose Down
DROP TABLE metrics;
