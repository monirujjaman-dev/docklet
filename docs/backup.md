# Backing up and restoring Docklet

Docklet's own state is small: `docklet.db` (projects, apps, domains, env vars,
database credentials, settings, users, audit log) and `master.key`, which
decrypts the secrets in that database. **Without the key, a database backup is
useless**, so every Docklet backup contains both.

Your apps' images and volumes are not part of it. Managed databases have
their own backups (Database → Backups). A Docklet backup can include the
newest one of each.

## Download a backup from the dashboard

**Settings → Back up Docklet.** Enter your password and an encryption
passphrase (12+ characters), then press **Download backup**. The file
(`docklet-<host>-<time>.dkbackup`) is encrypted with XChaCha20-Poly1305
under a key derived from the passphrase with argon2id. Store it off the
server, and keep the passphrase somewhere else: without it the file can't be
opened. Every download is recorded in the audit log.

## From the command line

Taking a backup is safe while Docklet runs: it snapshots the database with
`VACUUM INTO`.

```sh
cd /opt/docklet
# Stream an encrypted backup to the host:
docker compose exec -T -e DOCKLET_BACKUP_PASSPHRASE='your long passphrase' \
  docklet docklet backup -with-database-backups -o - > docklet-$(date +%F).dkbackup

# Check a backup (checksums of every file):
docker run --rm -i -e DOCKLET_BACKUP_PASSPHRASE='your long passphrase' \
  ghcr.io/monirujjaman-dev/docklet:latest restore -check -i - < docklet-2026-11-21.dkbackup
```

| Flag | Meaning |
|---|---|
| `-o FILE` / `-o -` | Output file, or stdout. Default: `<data-dir>/self-backups/docklet-<time>.dkbackup` |
| `-passphrase-file FILE` | Read the passphrase from a file instead of `DOCKLET_BACKUP_PASSPHRASE` |
| `-with-database-backups` | Include the newest successful backup of each managed database |
| `-no-encrypt` | Write a plain `.tar.gz` (it contains the master key — keep it on the server or encrypt it yourself) |

For a nightly backup, run the same command from the host's cron and copy the
file off the server, e.g. with `rclone` or `scp`.

### Automatic pre-upgrade backups

The installer takes a plain backup before every upgrade, keeps the three
newest in the `docklet-data` volume
(`/var/lib/docklet/self-backups/pre-upgrade-*.tar.gz`), and prints the file
name. They stay on the server, next to the key they contain.

## Restore

Docklet must be stopped while restoring: `restore` takes the same lock as
`serve` and refuses to run otherwise. It checks every file's checksum and the
database's integrity before touching anything. The current `docklet.db` and
`master.key` are then moved to `<data-dir>/pre-restore-<time>/`, not deleted.

```sh
cd /opt/docklet
docker compose stop docklet
docker compose run --rm --no-deps -e DOCKLET_BACKUP_PASSPHRASE='your long passphrase' \
  -v "$PWD/docklet-2026-11-21.dkbackup:/restore.dkbackup:ro" docklet restore -i /restore.dkbackup
docker compose start docklet
```

A backup from an **older** Docklet restores fine: missing migrations run on
start. A backup from a **newer** Docklet is refused; upgrade first.

### Moving to a new server

1. Install Docklet on the new server (same or newer version).
2. Restore the backup as above. Use `-with-database-backups` when taking it to
   bring database data along.
3. Point your DNS records at the new server.
4. In the dashboard, apps whose containers don't exist on this server show as
   **missing**: press **Deploy** on each. Databases show as missing too: press
   **Recreate** (it gets an empty volume), then restore the included backup
   under Backups.
5. Caddy requests new certificates for your domains on its own.
