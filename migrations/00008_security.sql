-- +goose Up
-- Sources allowed to reach a database's public port (comma separated IPv4
-- addresses / CIDRs). Empty = anyone. Enforced in iptables (DOCKER-USER).
ALTER TABLE databases ADD COLUMN public_allow TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE databases DROP COLUMN public_allow;
