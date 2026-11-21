# Backing up and restoring Docklet

Docklet's own state is small: `docklet.db` (projects, apps, domains, env vars,
database credentials, settings, users, audit log) and `master.key`, which
decrypts the secrets in that database. **Without the key, a database backup is
useless**, so every Docklet backup contains both.

Your apps' images and volumes are not part of it. Managed databases have
their own backups (Database → Backups). A Docklet backup can include the
newest one of each.

## Off-site backups to S3

Add a bucket under **Settings → Backup destinations (S3)**. Any S3-compatible
service works:

| Service | Endpoint | Region | Path-style |
|---|---|---|---|
| Amazon S3 | `https://s3.<region>.amazonaws.com` | the bucket's region | off |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` | on |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | e.g. `us-west-004` | off |
| Wasabi | `https://s3.<region>.wasabisys.com` | the bucket's region | off |
| DigitalOcean Spaces | `https://<region>.digitaloceanspaces.com` | e.g. `fra1` | off |
| MinIO, Garage, SeaweedFS | your server's URL | as configured | on |

- **Test connection** checks that the bucket exists, then writes, reads back
  and deletes a small test file. Use a key that can access only this bucket.
  The secret key is stored encrypted and never shown again.
- **Folder (prefix):** keys are stored under it. Database backups go to
  `<prefix>/databases/<id>-<name>/`, Docklet backups to `<prefix>/docklet/`.

### Managed databases

On a database's **Backups** tab, choose **Also upload to**:

- Every backup, manual or scheduled, is uploaded once it's taken. The upload
  is streamed in 16 MB parts, so large dumps don't use extra memory.
- The list shows where each backup is: *server*, *S3*, *uploading* or
  *upload failed* (with **retry**).
- **Keep N** applies to both copies: older backups are removed locally and
  from the bucket.
- **Download** and **Restore** use the local file when it exists, otherwise
  they fetch the copy from the bucket. That also covers a server whose disk
  was lost but whose Docklet backup was restored.
- Deleting a database never deletes its uploaded backups.

### Docklet itself

**Settings → Scheduled backups to S3** uploads an encrypted Docklet backup
daily or weekly and keeps the newest N. **Back up now** runs one immediately.

To run unattended, the passphrase is stored on the server (under the master
key, which is inside the backup). **Keep a copy of the passphrase elsewhere:**
it's what you'll need to restore on a new server. To restore, download the
newest `docklet/docklet-<host>-<time>.dkbackup` from the bucket and follow
[Restore](#restore).

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
