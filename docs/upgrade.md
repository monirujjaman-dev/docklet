# Upgrading Docklet

The dashboard checks for new releases when you open **Settings → Updates**.
Nothing is sent about your server; set `DOCKLET_UPDATE_CHECK=off` to disable
the check.

## Update now (container install)

**Settings → Updates → Update now** upgrades from the dashboard. Leave the version empty for the latest release, or type one (e.g. `1.4.1`) to pin it or go back to an older one. Your password is required.

1. Docklet writes a backup of its database and key to `self-backups/pre-upgrade-*.tar.gz` in the data volume (the three newest are kept). If the backup fails, nothing changes.
2. It pulls `ghcr.io/monirujjaman-dev/docklet:<version>`. If the pull fails but the image is already on the server, that image is used.
3. It starts a short-lived **updater** container from the *current* image. The updater:
   - stops Docklet and renames its container to `docklet-old-<time>`;
   - copies the SQLite files to `update/snapshot/`;
   - creates the new container with the same settings: env, volumes, ports, networks and the pinned address `172.30.0.3`. Values that only came from the old image (entrypoint, image env and labels) are left to the new image.
4. The updater waits up to 2 minutes for the new container to answer `/api/version` with the new version.
   - **Healthy:** the old container is removed and `DOCKLET_VERSION` in `/opt/docklet/.env` is updated, so a later `docker compose up -d` or installer run keeps the version.
   - **Not healthy** (it crashes, never answers, or reports another version): the updater records the last log lines, removes the new container, restores the database snapshot (the new version may have migrated it), and starts the previous container again.

The page shows each step live, including while Docklet restarts. The result is kept in `update/status.json` in the data volume, so the version that runs afterwards shows how it went. Apps, databases and Caddy keep running throughout; only the dashboard is away for a few seconds.

**Automatic patch updates** (optional, off by default): Docklet installs newer patch releases of the version it runs (1.4.0 → 1.4.2, never 1.5.0) during a one-hour maintenance window you pick (UTC). A version that rolled back is not retried automatically.

Update now needs the container install. On a binary install, use `sudo docklet upgrade` (below).

## Container install: re-run the installer

You can always upgrade from a shell instead:

```sh
curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh | sudo sh
```

It:

1. Picks the newest release. If `DOCKLET_VERSION` in `/opt/docklet/.env` is a
   channel such as `main`, it stays on that channel; pass `DOCKLET_VERSION=1.2.3`
   to pin a version.
2. Pulls the new images.
3. Stops Docklet for a moment and takes a backup of its database and key with
   the new image. If the backup fails, nothing changes.
4. Starts the new version.

Your sites keep serving the whole time: Caddy holds their routes, and app and
database containers aren't touched. Projects, apps, databases, domains, env
vars, cron jobs, sessions and the audit log are kept, and schema migrations
run on start.

Skip the backup with `DOCKLET_SKIP_BACKUP=1` (not recommended).

### Without the installer

```sh
cd /opt/docklet
sed -i 's/^DOCKLET_VERSION=.*/DOCKLET_VERSION=1.0.1/' .env
docker compose pull && docker compose up -d
```

### Rolling back

Set the previous version in `.env` and run `docker compose up -d`. If the
newer version had already migrated the database, the older one may refuse to
start. In that case, restore the pre-upgrade backup the installer printed (see
[backup.md](backup.md#restore)). The file is already in the volume:

```sh
cd /opt/docklet
docker compose stop docklet
docker compose run --rm --no-deps docklet restore -i /var/lib/docklet/self-backups/pre-upgrade-<time>.tar.gz
docker compose start docklet
```

## Binary install

If you run the `docklet` binary directly (systemd unit in
[`deploy/docklet.service`](../deploy/docklet.service)):

```sh
sudo docklet upgrade -check     # is there a newer release?
sudo docklet upgrade -restart   # install it and restart docklet.service
```

`docklet upgrade` does the following:

1. Downloads `docklet-linux-<arch>` from the GitHub release.
2. Verifies its SHA-256 against the release's `checksums.txt`.
3. Runs the new binary once (`version`) to make sure it starts.
4. Atomically replaces the old binary, which it keeps as `docklet.previous`.

If any step fails, nothing is changed. Use `-version v1.0.1` for a specific
release. Take a backup first (`docklet backup`); to roll back, move
`docklet.previous` back and restart.
