
-- name: ListDeploymentImageIDs :many
-- Images that deployments (current or kept for rollback) refer to.
SELECT DISTINCT image_id FROM deployments WHERE image_id <> '';
