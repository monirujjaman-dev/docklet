-- name: CreateCronJob :one
INSERT INTO cron_jobs (app_id, name, schedule, timezone, command, timeout_sec, concurrency, enabled, next_run_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetCronJob :one
SELECT * FROM cron_jobs WHERE id = ?;

-- name: ListAppCronJobs :many
SELECT * FROM cron_jobs WHERE app_id = ? ORDER BY name;

-- name: CountAppCronJobs :one
SELECT COUNT(*) FROM cron_jobs WHERE app_id = ?;

-- name: ListCronJobs :many
SELECT * FROM cron_jobs ORDER BY id;

-- name: ListDueCronJobs :many
SELECT * FROM cron_jobs WHERE enabled = 1 AND next_run_at > 0 AND next_run_at <= ? ORDER BY next_run_at;

-- name: UpdateCronJob :exec
UPDATE cron_jobs SET schedule = ?, timezone = ?, command = ?, timeout_sec = ?, concurrency = ?, enabled = ?, next_run_at = ?, updated_at = ?
WHERE id = ?;

-- name: SetCronNextRun :exec
UPDATE cron_jobs SET next_run_at = ? WHERE id = ?;

-- name: DeleteCronJob :exec
DELETE FROM cron_jobs WHERE id = ?;

-- name: CreateCronRun :one
INSERT INTO cron_runs (job_id, trigger, status, error, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: SetCronRunContainer :exec
UPDATE cron_runs SET container_id = ? WHERE id = ?;

-- name: FinishCronRun :exec
UPDATE cron_runs SET status = ?, exit_code = ?, error = ?, logs = ?, finished_at = ?, duration_ms = ? WHERE id = ?;

-- name: GetCronRun :one
SELECT * FROM cron_runs WHERE id = ?;

-- name: ListCronRuns :many
SELECT id, job_id, trigger, status, exit_code, container_id, error, started_at, finished_at, duration_ms
FROM cron_runs WHERE job_id = ? ORDER BY id DESC LIMIT ?;

-- name: LastCronRun :one
SELECT id, job_id, trigger, status, exit_code, container_id, error, started_at, finished_at, duration_ms
FROM cron_runs WHERE job_id = ? ORDER BY id DESC LIMIT 1;

-- name: CountRunningCronRuns :one
SELECT COUNT(*) FROM cron_runs WHERE job_id = ? AND status = 'running';

-- name: FailRunningCronRuns :execrows
UPDATE cron_runs SET status = 'failed', error = ?, finished_at = ? WHERE status = 'running';

-- name: PruneCronRuns :exec
-- Keeps the newest N runs of a job.
DELETE FROM cron_runs WHERE cron_runs.job_id = sqlc.arg(job_id) AND cron_runs.id NOT IN (
    SELECT r.id FROM cron_runs r WHERE r.job_id = sqlc.arg(job_id) ORDER BY r.id DESC LIMIT sqlc.arg(keep)
);
