-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (username, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: RecordFailedLogin :one
-- An expired lock starts a fresh count, so one typo after a lockout does not
-- immediately re-lock the account. Returns the new count.
UPDATE users
SET failed_logins = CASE WHEN locked_until > 0 AND locked_until <= sqlc.arg(now) THEN 1 ELSE failed_logins + 1 END,
    locked_until = CASE
        WHEN (CASE WHEN locked_until > 0 AND locked_until <= sqlc.arg(now) THEN 1 ELSE failed_logins + 1 END) >= sqlc.arg(max_attempts)
            THEN sqlc.arg(lock_until)
        WHEN locked_until <= sqlc.arg(now) THEN 0
        ELSE locked_until
    END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING failed_logins;

-- name: ResetFailedLogins :exec
UPDATE users SET failed_logins = 0, locked_until = 0, updated_at = ? WHERE id = ?;

-- name: UpdatePassword :exec
UPDATE users SET password_hash = ?, failed_logins = 0, locked_until = 0, updated_at = ? WHERE id = ?;

-- name: SetPendingTOTP :exec
UPDATE users SET totp_secret_enc = ?, totp_enabled = 0, updated_at = ? WHERE id = ?;

-- name: EnableTOTP :exec
UPDATE users SET totp_enabled = 1, totp_last_step = ?, updated_at = ? WHERE id = ?;

-- name: DisableTOTP :exec
UPDATE users SET totp_secret_enc = NULL, totp_enabled = 0, totp_last_step = 0, updated_at = ? WHERE id = ?;

-- name: SetTOTPLastStep :exec
UPDATE users SET totp_last_step = ?, updated_at = ? WHERE id = ?;
