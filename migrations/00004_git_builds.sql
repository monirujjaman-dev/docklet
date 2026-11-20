-- +goose Up
-- source_type: 'image' (pull a registry image) or 'git' (clone + docker build).
ALTER TABLE apps ADD COLUMN source_type TEXT NOT NULL DEFAULT 'image';
ALTER TABLE apps ADD COLUMN git_url TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN git_branch TEXT NOT NULL DEFAULT 'main';
ALTER TABLE apps ADD COLUMN build_context TEXT NOT NULL DEFAULT '.';
ALTER TABLE apps ADD COLUMN dockerfile TEXT NOT NULL DEFAULT 'Dockerfile';
-- SSH deploy key: private half encrypted with the master key.
ALTER TABLE apps ADD COLUMN deploy_key_enc BLOB;
ALTER TABLE apps ADD COLUMN deploy_key_pub TEXT NOT NULL DEFAULT '';
-- Webhook: the token identifies the app in the URL; the secret signs payloads.
ALTER TABLE apps ADD COLUMN webhook_token TEXT;
ALTER TABLE apps ADD COLUMN webhook_secret_enc BLOB;
ALTER TABLE apps ADD COLUMN auto_deploy INTEGER NOT NULL DEFAULT 1;
CREATE UNIQUE INDEX apps_webhook_token ON apps(webhook_token) WHERE webhook_token IS NOT NULL;

-- source: 'image', 'git' or 'rollback'. image_pruned marks deployments whose
-- built image was removed by cleanup (no longer available for rollback).
ALTER TABLE deployments ADD COLUMN source TEXT NOT NULL DEFAULT 'image';
ALTER TABLE deployments ADD COLUMN commit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN commit_message TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN rollback_of INTEGER;
ALTER TABLE deployments ADD COLUMN image_pruned INTEGER NOT NULL DEFAULT 0;

-- +goose Down
DROP INDEX apps_webhook_token;
ALTER TABLE deployments DROP COLUMN image_pruned;
ALTER TABLE deployments DROP COLUMN rollback_of;
ALTER TABLE deployments DROP COLUMN commit_message;
ALTER TABLE deployments DROP COLUMN commit_sha;
ALTER TABLE deployments DROP COLUMN source;
ALTER TABLE apps DROP COLUMN auto_deploy;
ALTER TABLE apps DROP COLUMN webhook_secret_enc;
ALTER TABLE apps DROP COLUMN webhook_token;
ALTER TABLE apps DROP COLUMN deploy_key_pub;
ALTER TABLE apps DROP COLUMN deploy_key_enc;
ALTER TABLE apps DROP COLUMN dockerfile;
ALTER TABLE apps DROP COLUMN build_context;
ALTER TABLE apps DROP COLUMN git_branch;
ALTER TABLE apps DROP COLUMN git_url;
ALTER TABLE apps DROP COLUMN source_type;
