-- name: UpsertMetric :exec
INSERT INTO metrics (scope, ts, cpu, mem_used, mem_limit, disk_used, disk_total, net_rx, net_tx, blk_read, blk_write)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (scope, ts) DO UPDATE SET
    cpu = excluded.cpu, mem_used = excluded.mem_used, mem_limit = excluded.mem_limit,
    disk_used = excluded.disk_used, disk_total = excluded.disk_total,
    net_rx = excluded.net_rx, net_tx = excluded.net_tx,
    blk_read = excluded.blk_read, blk_write = excluded.blk_write;

-- name: ListMetrics :many
SELECT * FROM metrics WHERE scope = sqlc.arg(scope) AND ts >= sqlc.arg(from_ts) AND ts <= sqlc.arg(to_ts) ORDER BY ts;

-- name: PruneMetrics :execrows
DELETE FROM metrics WHERE ts < ?;

-- name: ListAppContainers :many
-- Current container of each app that should be running.
SELECT a.id AS app_id, a.name, d.container_id
FROM apps a
JOIN deployments d ON d.id = a.current_deployment_id
WHERE d.container_id != '' AND a.desired_status = 'running';
