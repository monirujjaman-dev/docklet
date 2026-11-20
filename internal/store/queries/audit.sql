-- name: LastAuditEntry :one
SELECT id, hash FROM audit_log ORDER BY id DESC LIMIT 1;

-- name: InsertAuditEntry :exec
INSERT INTO audit_log (id, ts, actor_id, actor, action, target, ip, details, prev_hash, hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListAuditEntries :many
SELECT * FROM audit_log
WHERE id < sqlc.arg(before_id)
ORDER BY id DESC
LIMIT sqlc.arg(limit);

-- name: AuditEntriesFrom :many
SELECT * FROM audit_log
WHERE id > sqlc.arg(after_id)
ORDER BY id ASC
LIMIT sqlc.arg(limit);
