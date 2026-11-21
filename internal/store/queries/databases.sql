-- name: CreateDatabase :one
INSERT INTO databases (project_id, name, engine, version, db_name, username, password_enc, root_password_enc,
                       memory_mb, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetDatabase :one
SELECT * FROM databases WHERE id = ?;

-- name: ListDatabases :many
SELECT * FROM databases ORDER BY project_id, name;

-- name: CountProjectDatabases :one
SELECT COUNT(*) FROM databases WHERE project_id = ?;

-- name: SetDatabaseContainer :exec
UPDATE databases SET container_id = ?, updated_at = ? WHERE id = ?;

-- name: SetDatabaseRuntime :exec
UPDATE databases SET runtime_status = ?, runtime_message = ? WHERE id = ?;

-- name: SetDatabaseDesired :exec
UPDATE databases SET desired_status = ?, updated_at = ? WHERE id = ?;

-- name: SetDatabasePublicPort :exec
UPDATE databases SET public_port = ?, updated_at = ? WHERE id = ?;

-- name: SetDatabaseBackupPolicy :exec
UPDATE databases SET backup_schedule = ?, backup_keep = ?, updated_at = ? WHERE id = ?;

-- name: DeleteDatabase :exec
DELETE FROM databases WHERE id = ?;

-- name: PublicPortInUse :one
SELECT COUNT(*) FROM databases WHERE public_port = ? AND id != ?;

-- name: CreateBackup :one
INSERT INTO backups (database_id, trigger, started_at) VALUES (?, ?, ?) RETURNING *;

-- name: FinishBackup :exec
UPDATE backups SET status = ?, file = ?, size_bytes = ?, error = ?, finished_at = ? WHERE id = ?;

-- name: GetBackup :one
SELECT * FROM backups WHERE id = ?;

-- name: ListBackups :many
SELECT * FROM backups WHERE database_id = ? ORDER BY id DESC LIMIT ?;

-- name: LastBackupTime :one
-- Start time of the newest scheduled or manual backup that did not fail.
SELECT CAST(COALESCE(MAX(started_at), 0) AS INTEGER) FROM backups
WHERE database_id = ? AND status != 'failed' AND trigger != 'pre-restore';

-- name: ListExpiredBackups :many
-- Successful backups beyond the newest `keep`.
SELECT * FROM backups WHERE database_id = sqlc.arg(database_id) AND status = 'succeeded'
ORDER BY id DESC LIMIT -1 OFFSET sqlc.arg(keep);

-- name: DeleteBackup :exec
DELETE FROM backups WHERE id = ?;

-- name: FailRunningBackups :execrows
UPDATE backups SET status = 'failed', error = 'interrupted by a restart', finished_at = ? WHERE status = 'running';

-- name: UpsertDatabaseLink :exec
INSERT INTO database_links (database_id, app_id, env_key, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT (database_id, app_id) DO UPDATE SET env_key = excluded.env_key;

-- name: DeleteDatabaseLink :exec
DELETE FROM database_links WHERE database_id = ? AND app_id = ?;

-- name: ListDatabaseLinks :many
SELECT l.app_id, l.env_key, a.name AS app_name
FROM database_links l JOIN apps a ON a.id = l.app_id
WHERE l.database_id = ? ORDER BY a.name;
