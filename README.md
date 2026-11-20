# Docklet

A lightweight, self-hosted PaaS for a single VPS, built directly on **Docker** and **Caddy**.
One Go binary, SQLite, zero-config TLS, and a lean control plane so your server's RAM goes to your apps.

> Status: **M3 — deploy apps from Docker images**. Git builds, databases and metrics come next.

## Goals

- Deploy apps from images, Dockerfiles or Git, with rollback
- One-click PostgreSQL / MySQL / Redis with backups
- Workers and cron jobs
- Live host and per-container metrics and logs
- Standalone auth and tamper-evident audit log
- Idle footprint: Docklet ≤ 40 MB, Docklet + Caddy ≤ 80 MB

## Stack

Go (stdlib `net/http`) · Docker Engine API · Caddy Admin API · SQLite · Svelte 5 + Vite + Tailwind (embedded into the binary)

## Install

On a Linux server (installs Docker if needed, then Docklet + Caddy as containers):

```sh
curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh | sudo sh
```

See [docs/install.md](docs/install.md). To try it on Windows or macOS with Docker Desktop, see [docs/docker-desktop.md](docs/docker-desktop.md) (`docker compose up -d --build`).

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
./bin/docklet admin clear-panel-domain -data-dir /var/lib/docklet
./bin/docklet version
```

The first visit to the dashboard shows a one-time setup page for the admin account.

### Apps

Create a **project** (each gets a private Docker bridge network) and add **apps** from an image (`nginx:alpine`, `ghcr.io/org/app:1.2`). Each deploy goes through these steps:

1. pull the image (if the registry can't be reached, a copy already on the host is used)
2. create the container with its encrypted env vars and CPU/memory limits, and start it
3. wait until its port accepts TCP connections
4. point Caddy at the new container
5. after a short drain, stop the previous container

If any step fails, the new container is removed and the previous version keeps serving. Docklet routes Caddy straight to the container's IP on the project network, so it doesn't publish host ports and no `docker-proxy` processes run. Containers reach each other by app name. Live logs are streamed over Server-Sent Events.

Every 30 s Docklet compares Docker with its database:
- containers removed outside Docklet show up as *missing*
- if Docker restarted and a container's IP changed, Caddy is updated to the new IP
- containers left behind by failed or replaced deployments are cleaned up

Interrupted deploy jobs resume when Docklet restarts.

### Caddy and the panel domain

Docklet manages [Caddy](https://caddyserver.com) through its Admin API (`-caddy-admin`, default `http://localhost:2019`; `off` disables it). Caddy runs as its own process (`caddy run`), and Docklet owns its entire config: the database is the source of truth, and the full config is re-applied within ~10 s whenever Caddy restarts or drifts. If Docklet stops, Caddy keeps serving.

In **Settings → Panel domain**, enter a domain whose DNS points at the server (ports 80/443 open). Caddy gets a certificate, and Docklet checks that `https://<domain>` really reaches it. Only after that check passes does Docklet bind its port to `127.0.0.1`, so a DNS or firewall mistake can't lock you out. If the domain stops working later, run `docklet admin clear-panel-domain` and restart Docklet to get direct access back.

The data directory holds `docklet.db` (SQLite) and `master.key`, which encrypts secrets at rest. Back up **both**: without the key, encrypted values such as 2FA secrets cannot be recovered.

Opt-in integration tests: `DOCKLET_TEST_DOCKER=1 go test ./internal/docker/` (needs a local image `docklet-test/hello:v1` listening on 8080) and `DOCKLET_TEST_CADDY_ADMIN=http://localhost:2019 go test ./internal/caddy/`.

After editing SQL in `migrations/` or `internal/store/queries/`, run `make sqlc` ([sqlc](https://sqlc.dev) v1.29+).

## Layout

```
cmd/docklet/        CLI entrypoint (serve, audit verify, admin reset-password, version)
internal/server/    HTTP API, sessions, CSRF, rate limiting, panel domain, embedded SPA (dist/)
internal/caddy/     Caddy config builder, Admin API client, reconciler
internal/apps/      deploy pipeline, job workers, lifecycle, Docker reconciler
internal/docker/    minimal Docker Engine API client (stdlib only); dockertest/ is an in-memory fake
internal/auth/      argon2id, TOTP (RFC 6238), tokens
internal/audit/     append-only hash-chained audit log + verifier
internal/secrets/   XChaCha20-Poly1305 encryption with the master key
internal/store/     SQLite (WAL) + goose migrations; db/ is sqlc-generated
migrations/         SQL migrations (embedded)
internal/version/   build metadata (set via -ldflags)
web/                Svelte 5 dashboard (built into internal/server/dist)
```
