-- name: CreateProject :one
INSERT INTO projects (name, created_at) VALUES (?, ?) RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY name;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = ?;

-- name: CountProjectApps :one
SELECT COUNT(*) FROM apps WHERE project_id = ?;

-- name: CreateApp :one
INSERT INTO apps (project_id, name, image, port, restart_policy, cpu_millis, memory_mb, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetApp :one
SELECT * FROM apps WHERE id = ?;

-- name: ListApps :many
SELECT * FROM apps ORDER BY project_id, name;

-- name: ListProjectApps :many
SELECT * FROM apps WHERE project_id = ? ORDER BY name;

-- name: UpdateAppConfig :exec
UPDATE apps SET image = ?, port = ?, restart_policy = ?, cpu_millis = ?, memory_mb = ?, updated_at = ? WHERE id = ?;

-- name: SetAppDesiredStatus :exec
UPDATE apps SET desired_status = ?, updated_at = ? WHERE id = ?;

-- name: SetAppRuntime :exec
UPDATE apps SET runtime_status = ?, runtime_message = ? WHERE id = ?;

-- name: SetAppCurrentDeployment :exec
UPDATE apps SET current_deployment_id = ?, updated_at = ? WHERE id = ?;

-- name: DeleteApp :exec
DELETE FROM apps WHERE id = ?;

-- name: CreateDeployment :one
INSERT INTO deployments (app_id, image, created_by, created_at) VALUES (?, ?, ?, ?) RETURNING *;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE id = ?;

-- name: ListAppDeployments :many
SELECT id, app_id, image, image_id, port, status, container_id, container_ip, error, created_by, created_at, started_at, finished_at
FROM deployments WHERE app_id = ? ORDER BY id DESC LIMIT ?;

-- name: StartDeployment :exec
UPDATE deployments SET status = 'running', started_at = ?, error = '', log = '' WHERE id = ?;

-- name: SetDeploymentContainer :exec
UPDATE deployments SET container_id = ?, container_ip = ?, image_id = ?, port = ? WHERE id = ?;

-- name: SetDeploymentIP :exec
UPDATE deployments SET container_ip = ? WHERE id = ?;

-- name: FinishDeployment :exec
UPDATE deployments SET status = ?, error = ?, log = ?, finished_at = ? WHERE id = ?;

-- name: SetDeploymentStatus :exec
UPDATE deployments SET status = ? WHERE id = ?;

-- name: ListEnv :many
SELECT * FROM env_vars WHERE app_id = ? ORDER BY key;

-- name: UpsertEnv :exec
INSERT INTO env_vars (app_id, key, value_enc, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (app_id, key) DO UPDATE SET value_enc = excluded.value_enc, updated_at = excluded.updated_at;

-- name: DeleteEnv :exec
DELETE FROM env_vars WHERE app_id = ? AND key = ?;

-- name: ListAppDomains :many
SELECT domain FROM domains WHERE app_id = ? ORDER BY domain;

-- name: GetDomainOwner :one
SELECT app_id FROM domains WHERE domain = ?;

-- name: InsertDomain :exec
INSERT INTO domains (domain, app_id, created_at) VALUES (?, ?, ?);

-- name: DeleteAppDomains :exec
DELETE FROM domains WHERE app_id = ?;

-- name: ListRoutableDomains :many
-- Domains of running apps whose current deployment has a reachable container.
SELECT d.domain, a.id AS app_id, dep.container_ip, dep.port
FROM domains d
JOIN apps a ON a.id = d.app_id
JOIN deployments dep ON dep.id = a.current_deployment_id
WHERE a.desired_status = 'running' AND dep.container_ip != '' AND dep.port > 0
ORDER BY d.domain;

-- name: InsertJob :one
INSERT INTO jobs (kind, ref_id, created_at) VALUES (?, ?, ?) RETURNING id;

-- name: ClaimJob :one
UPDATE jobs SET status = 'running', attempts = attempts + 1, started_at = sqlc.arg(now)
WHERE id = (SELECT j.id FROM jobs j WHERE j.status = 'queued' ORDER BY j.id LIMIT 1)
RETURNING *;

-- name: FinishJob :exec
UPDATE jobs SET status = ?, error = ?, finished_at = ? WHERE id = ?;

-- name: RequeueRunningJobs :execrows
UPDATE jobs SET status = 'queued' WHERE status = 'running';
