# Troubleshooting

Commands assume the container install in `/opt/docklet`. For a binary
install, drop the `docker compose exec docklet` prefix.

## First steps

```sh
cd /opt/docklet
docker compose ps                         # both containers Up?
docker compose logs --tail 100 docklet    # errors are logged with err=…
docker compose logs --tail 100 caddy      # certificate / ACME problems
curl -fsS http://127.0.0.1:3000/api/health
```

## Installation

| Symptom | Cause and fix |
|---|---|
| `port(s) 80 443 already in use` | Another web server or platform (nginx, Traefik, Dokploy, Coolify) holds the ports. Stop it, or install on other ports for testing (see [install.md](install.md#on-a-server-that-already-runs-dokploy-or-coolify-nginx)) |
| `Docker network '…' already uses 172.30.0.0/24` | Remove or re-address that network; Docklet needs the subnet for Caddy's admin API |
| `could not pull images` | ghcr.io or Docker Hub is unreachable from the server; check DNS, firewall or proxy |
| `Docklet did not become healthy` | The installer prints the last log lines. Most often the data volume belongs to a newer version (see below) |

## Dashboard

| Symptom | Cause and fix |
|---|---|
| Locked out after a wrong password | Five failures lock the account for 15 minutes. Or: `docker compose exec docklet docklet admin reset-password -username admin` |
| Lost the 2FA device | `docker compose exec docklet docklet admin reset-password -username admin -disable-2fa` |
| Panel domain set, but HTTPS doesn't come up and the dashboard is unreachable | `docker compose exec docklet docklet admin clear-panel-domain && docker compose restart docklet`, then check the DNS record and that ports 80/443 reach Caddy |
| "Builds are unavailable on this server" | The image ships git, ssh and buildx. On a binary install, install `git`, `openssh-client` and the Docker CLI with buildx |

## Apps

| Symptom | Cause and fix |
|---|---|
| Deploy fails at "Waiting for port … to accept connections" | The app listens on another port, or only on 127.0.0.1. Set the right **Port** and make it listen on `0.0.0.0`. The deployment log ends with the container's own output |
| Worker deploy fails with "exited with code N" | The process stops on its own (e.g. a missing env var). See its output in the deployment log |
| App shows **missing** | Its container was removed outside Docklet; press **Deploy** |
| Status says "Restarted N time(s)" | The process crashed and Docker restarted it. Check **Logs** |
| Domain has no certificate | DNS must point at this server, and ports 80/443 must reach Caddy. See `docker compose logs caddy` |

## Databases and cron

| Symptom | Cause and fix |
|---|---|
| App can't connect with `DATABASE_URL` | Redeploy the app after linking. The app and the database must be in the same project |
| Restore failed | The database keeps running with its previous data. A "pre-restore" backup was taken first; the error is in the backup list |
| Cron run "skipped" | The previous run was still going and the job's policy is *skip*. Raise the interval, or choose *allow* |
| Cron run fails with "no successful deployment" | Deploy the app once; runs use the image of its current deployment |

## Docklet won't start after an upgrade or restore

- **`the data directory is in use`**: another Docklet (or a `restore`) is using
  the same data directory. Stop it.
- **migration errors**: the data comes from a newer version. Use that version
  again, or restore a pre-upgrade backup ([upgrade.md](upgrade.md#rolling-back)).
- **`master key` errors**: `master.key` is missing or doesn't match the
  database. Restore both from the same backup.

## Reporting a bug

Include `docker compose exec docklet docklet version`, the relevant log lines,
and what you did. Remove domains, IPs and secrets you don't want to share.
