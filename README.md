# Docklet

A lightweight, self-hosted PaaS for a single VPS, built directly on **Docker** and **Caddy**.
One Go binary, SQLite, zero-config TLS, and a lean control plane so your server's RAM goes to your apps.

> Status: **v1.0** — Docklet + Caddy idle at 63.5 MB RSS, vs 754 MB for Dokploy's control plane ([benchmarks](docs/benchmarks.md)).

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

See [install](docs/install.md), [upgrade](docs/upgrade.md), [backup and restore](docs/backup.md), [troubleshooting](docs/troubleshooting.md) and [security](docs/security.md). To try it on Windows or macOS with Docker Desktop, see [docs/docker-desktop.md](docs/docker-desktop.md) (`docker compose up -d --build`).

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

### Git builds, webhooks and rollback

An app can deploy from a **Git repository** instead of a registry image. For each deploy, Docklet:
1. clones the branch (shallow);
2. builds the repository's Dockerfile with the docker CLI (BuildKit), so cache mounts and `# syntax=` work;
3. tags the image `docklet/<project>-<app>:d<N>` and deploys it like any other image.

Build output streams into the deployment log while it runs. By default one build runs at a time, each with a 20 minute limit (`-build-timeout`). Builds need `git`, `openssh-client` and the docker CLI with buildx on the host; the container image includes them.

- **Private repositories:** use an SSH URL (`git@github.com:org/repo.git`). Each app gets its own ed25519 **deploy key**: add the public key to the repository as a read-only deploy key. URLs that contain credentials are rejected.
- **Webhooks:** every Git app has a webhook URL and secret. Add a *push* webhook in GitHub, Gitea/Forgejo or GitLab, with content type `application/json`. Docklet checks `X-Hub-Signature-256`, `X-Gitea-Signature` or `X-Gitlab-Token` and deploys pushes to the app's branch.
- **Rollback:** any earlier successful deployment whose image still exists can be redeployed as-is, with no build or pull.
- **Image cleanup:** Docklet keeps the 5 newest built images per app and removes older ones.

Builds without a Dockerfile (Railpack) are not supported yet.

### Databases

**New database** on a project provisions PostgreSQL (17/16/18), MySQL 8.4, MariaDB 11.4 or Redis 7.4:

- **Credentials.** A 28-character password (plus a root password for MySQL/MariaDB) is generated and stored encrypted. Revealing it in the dashboard is audited.
- **Storage.** Data lives in a named volume, `docklet-db-<id>`, so it survives container recreation, restarts and Docklet upgrades.
- **Network.** The container joins the project network under its own name (`postgres://main:…@main:5432/main`). No host port is published unless you enable **Public access** with an explicit port; that change is audited and recreates the container on the same volume.
- **Link to app.** This writes the connection URL into an app's encrypted env, as `DATABASE_URL` or `REDIS_URL` by default. Redeploy the app to pick it up. Unlinking or deleting the database removes the variable again.
- **Backups.** These use `pg_dump -Fc`, `mysqldump`/`mariadb-dump` (gzipped) or a Redis RDB snapshot. They are written to `<data-dir>/backups/<id>/` and can run on demand or on a schedule (off, hourly, daily or weekly; daily with keep 7 is the default).
- **Restore.** Restoring first takes a safety `pre-restore` backup and aborts if that fails.
- **Download.** Any backup can be downloaded from the dashboard.

A database name is unique per project, across both apps and databases, since both are DNS names on the same network. Deleting a database keeps its volume and backups unless you choose to delete the data too.

### Workers and cron jobs

An app is either a **web service** (gets domains and HTTPS through Caddy) or a **background worker**:

- **No route.** A worker has no domains and never appears in Caddy. Deploy succeeds once the process has kept running for 5 s. If you set an internal port, deploy instead waits until it accepts TCP connections.
- **Service discovery.** Other apps in the project reach it by name (e.g. `jobs:9000`), the same way they reach any app or database there.
- **Crashes.** Docker restarts the container per its restart policy (`unless-stopped` by default). The app page shows the policy and how often the container has been restarted.
- **Command.** Either kind can override the image's CMD with a command such as `php artisan queue:work`. The command is split like a shell would split it, but runs without a shell; use `sh -c '…'` for pipes, `&&` or `$VARS`.

**Cron jobs** belong to an app. On each run Docklet starts a one-off container from the image of the app's *current deployment*, with the app's environment (as it is now), the project network and its resource limits. When the run ends, the output is saved and the container is removed.

- **Schedule:** standard 5-field cron (`*/5 * * * *`, `0 3 * * mon-fri`, `@daily`), evaluated in a time zone you choose. The dashboard previews the next run times.
- **Timeout:** a run that exceeds its timeout (default 1 h) is stopped and marked `timeout`.
- **Concurrency:** with `skip` (the default), a run that comes due while the previous one is still going is recorded as `skipped`. With `allow`, the runs overlap.
- **History:** the last 100 runs per job are kept, each with status, exit code, duration and the last 64 KB of output. You can **Run now**, pause, or stop a run in progress.
- **Downtime:** runs missed while Docklet was down are not caught up; the next run is the next scheduled time. A run interrupted by a Docklet restart is marked failed.

Creating, changing, deleting, running and cancelling jobs is audited. Scheduled runs are not audited, but each one appears in the run history.

### Monitoring

**Monitoring** shows live host CPU, memory, disk and network (sampled every 2 s) and a per-app table. Each app also has a **Metrics** tab. Live views cover the last 30 minutes and stream over SSE; the 1 h, 6 h, 24 h and 7 d views come from one-minute averages stored in SQLite (kept for `-metrics-retention-days`, default 7). Partial minutes are written on shutdown, so a restart loses no history.

Where the numbers come from:
- **Host:** `/proc` and `statfs`
- **Containers:**
  - cgroup v2 files are read directly when Docklet can see them, which costs nothing per sample;
  - otherwise Docklet falls back to Docker's one-shot stats API, sampled every 6 s (cgroup v1 hosts, or Docker Desktop without the host mounts).

With 20 containers on the stats fallback, collection added 0.13% of one core and 2 MB RSS to Docklet. In containers, mount the host's `/proc` and `/sys/fs/cgroup` read-only and set `DOCKLET_HOST_PROC` and `DOCKLET_HOST_CGROUP`; `compose.yaml` already does this.

### Caddy and the panel domain

Docklet manages [Caddy](https://caddyserver.com) through its Admin API (`-caddy-admin`, default `http://localhost:2019`; `off` disables it). Caddy runs as its own process (`caddy run`), and Docklet owns its entire config: the database is the source of truth, and the full config is re-applied within ~10 s whenever Caddy restarts or drifts. If Docklet stops, Caddy keeps serving.

In **Settings → Panel domain**, enter a domain whose DNS points at the server (ports 80/443 open). Caddy gets a certificate, and Docklet checks that `https://<domain>` really reaches it. Only after that check passes does Docklet bind its port to `127.0.0.1`, so a DNS or firewall mistake can't lock you out. If the domain stops working later, run `docklet admin clear-panel-domain` and restart Docklet to get direct access back.

The data directory holds `docklet.db` (SQLite) and `master.key`, which encrypts secrets at rest. Back up **both** with **Settings → Back up Docklet** or `docklet backup` ([docs/backup.md](docs/backup.md)): without the key, encrypted values such as 2FA secrets cannot be recovered.

### CLI

```
docklet serve                      run the control plane
docklet backup [-o file]           encrypted backup of the database + master key (safe while running)
docklet restore -i file [-check]   restore one (Docklet stopped); -check only verifies it
docklet upgrade [-check]           binary installs: download the latest release, verify its checksum, swap
docklet audit verify               check the audit log's hash chain
docklet admin reset-password       reset a password (-disable-2fa to turn off 2FA)
docklet admin clear-panel-domain   regain direct access if the panel domain broke
docklet version
```

Releases (tag `vX.Y.Z`) publish static `docklet-linux-{amd64,arm64}` binaries with `checksums.txt`, and the image `ghcr.io/monirujjaman-dev/docklet:X.Y.Z`. `go run ./scripts/bench` checks the performance budgets ([docs/benchmarks.md](docs/benchmarks.md)).

Opt-in integration tests: `DOCKLET_TEST_DOCKER=1 go test ./internal/docker/` (needs a local image `docklet-test/hello:v1` listening on 8080) and `DOCKLET_TEST_CADDY_ADMIN=http://localhost:2019 go test ./internal/caddy/`.

After editing SQL in `migrations/` or `internal/store/queries/`, run `make sqlc` ([sqlc](https://sqlc.dev) v1.29+).

## Layout

```
cmd/docklet/        CLI entrypoint (serve, backup, restore, upgrade, audit verify, admin …, version)
internal/server/    HTTP API, sessions, CSRF, rate limiting, panel domain, embedded SPA (dist/)
internal/caddy/     Caddy config builder, Admin API client, reconciler
internal/apps/      deploy pipeline (pull / build / rollback), job workers, lifecycle, Docker reconciler, image cleanup
internal/build/     git clone + docker build (BuildKit) with streamed logs; deploy keys
internal/metrics/   host (/proc) and container (cgroup v2 or Docker stats) sampling, ring buffers, minute rollups
internal/docker/    minimal Docker Engine API client (stdlib only); dockertest/ is an in-memory fake
internal/auth/      argon2id, TOTP (RFC 6238), tokens
internal/audit/     append-only hash-chained audit log + verifier
internal/secrets/   XChaCha20-Poly1305 encryption with the master key
internal/store/     SQLite (WAL) + goose migrations; db/ is sqlc-generated
internal/databases/ managed PostgreSQL / MySQL / MariaDB / Redis, backups and restore
internal/cron/      cron parser and scheduler (one-off containers)
internal/selfbackup/ encrypted export/restore of docklet.db + master.key, data-dir lock
internal/release/   GitHub release lookup and checksum-verified binary install
scripts/bench/      budget benchmark
migrations/         SQL migrations (embedded)
internal/version/   build metadata (set via -ldflags)
web/                Svelte 5 dashboard (built into internal/server/dist)
```
