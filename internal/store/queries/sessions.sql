-- name: CreateSession :exec
INSERT INTO sessions (id, user_id, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = ? WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = ?;

-- name: DeleteUserSessionsExcept :exec
DELETE FROM sessions WHERE user_id = ? AND id != ?;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= sqlc.arg(now) OR last_seen_at <= sqlc.arg(idle_before);
