# Docklet

A lightweight, self-hosted PaaS for a single VPS, built directly on **Docker** and **Caddy**.
One Go binary, SQLite, zero-config TLS, and a lean control plane so your server's RAM goes to your apps.

> Status: **M1 — auth & audit**. No app deployment yet.

## Goals

- Deploy apps from images, Dockerfiles or Git, with rollback
- One-click PostgreSQL / MySQL / Redis with backups
- Workers and cron jobs
- Live host and per-container metrics and logs
- Standalone auth and tamper-evident audit log
- Idle footprint: Docklet ≤ 40 MB, Docklet + Caddy ≤ 80 MB

## Stack

Go (stdlib `net/http`) · Docker Engine API · Caddy Admin API · SQLite · Svelte 5 + Vite + Tailwind (embedded into the binary)

## Development

Requirements: Go 1.24+, Node 22+, make.

```sh
make dev     # Go API on :3000 + Vite on :5173 (open http://localhost:5173)
make build   # dashboard + static binary at ./bin/docklet
make test    # Go tests + frontend type-check
make lint
```

Run the binary:

```sh
./bin/docklet serve -addr :3000 -data-dir /var/lib/docklet
./bin/docklet audit verify -data-dir /var/lib/docklet
./bin/docklet admin reset-password -data-dir /var/lib/docklet -username admin [-disable-2fa]
./bin/docklet version
```

The first visit to the dashboard shows a one-time setup page for the admin account.

The data directory holds `docklet.db` (SQLite) and `master.key`, which encrypts secrets at rest. Back up **both**: without the key, encrypted values such as 2FA secrets cannot be recovered.

After editing SQL in `migrations/` or `internal/store/queries/`, run `make sqlc` ([sqlc](https://sqlc.dev) v1.29+).

## Layout

```
cmd/docklet/        CLI entrypoint (serve, audit verify, admin reset-password, version)
internal/server/    HTTP API, sessions, CSRF, rate limiting, embedded SPA (dist/)
internal/auth/      argon2id, TOTP (RFC 6238), tokens
internal/audit/     append-only hash-chained audit log + verifier
internal/secrets/   XChaCha20-Poly1305 encryption with the master key
internal/store/     SQLite (WAL) + goose migrations; db/ is sqlc-generated
migrations/         SQL migrations (embedded)
internal/version/   build metadata (set via -ldflags)
web/                Svelte 5 dashboard (built into internal/server/dist)
```
