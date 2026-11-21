-- name: CreateDestination :one
INSERT INTO s3_destinations (name, endpoint, region, bucket, prefix, access_key, secret_enc, path_style, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, x'', ?, ?, ?)
RETURNING *;

-- name: SetDestinationSecret :exec
UPDATE s3_destinations SET secret_enc = ? WHERE id = ?;

-- name: UpdateDestination :exec
UPDATE s3_destinations SET name = ?, endpoint = ?, region = ?, bucket = ?, prefix = ?, access_key = ?, path_style = ?, updated_at = ?
WHERE id = ?;

-- name: SetDestinationTest :exec
UPDATE s3_destinations SET last_test_at = ?, last_test_error = ? WHERE id = ?;

-- name: GetDestination :one
SELECT * FROM s3_destinations WHERE id = ?;

-- name: ListDestinations :many
SELECT * FROM s3_destinations ORDER BY name;

-- name: DeleteDestination :exec
DELETE FROM s3_destinations WHERE id = ?;

-- name: CountDestinationUsers :one
SELECT CAST((SELECT COUNT(*) FROM databases WHERE backup_destination_id = sqlc.arg(id))
          + (SELECT COUNT(*) FROM settings WHERE key = 'self_backup.destination' AND value = CAST(sqlc.arg(id) AS TEXT)) AS INTEGER);

-- name: SetDatabaseDestination :exec
UPDATE databases SET backup_destination_id = ?, updated_at = ? WHERE id = ?;

-- name: SetBackupRemote :exec
UPDATE backups SET destination_id = ?, remote_key = ?, remote_status = ?, remote_error = ? WHERE id = ?;

-- name: SetBackupFile :exec
UPDATE backups SET file = ? WHERE id = ?;

-- name: CreateDatabaseUser :one
INSERT INTO database_users (database_id, username, password_enc, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetDatabaseUser :one
SELECT * FROM database_users WHERE id = ?;

-- name: ListDatabaseUsers :many
SELECT * FROM database_users WHERE database_id = ? ORDER BY username;

-- name: UpdateDatabaseUser :exec
UPDATE database_users SET password_enc = ?, role = ?, updated_at = ? WHERE id = ?;

-- name: DeleteDatabaseUser :exec
DELETE FROM database_users WHERE id = ?;
