# Installing Docklet on a Linux server

One command installs Docker (if needed), Docklet and Caddy:

```sh
curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh | sudo sh
```

Then open `http://<server-ip>:3000` and **create the admin account immediately**:
until it exists, anyone who can reach that port can claim it.

## Requirements

- Linux on x86_64 or arm64 (Ubuntu 22.04+/Debian 12+ recommended), 1 GB RAM
- Root access (`sudo`)
- Ports **80** and **443** free for Caddy, and **3000** for the dashboard
- A domain pointing at the server if you want HTTPS for the panel and apps

## What it does

1. Installs Docker with `get.docker.com` if it's missing, and checks for
   Docker 25+ with the Compose plugin.
2. Stops if ports 80/443/3000 or the `172.30.0.0/24` Docker subnet are
   already in use, rather than breaking another service.
3. Downloads `compose.yaml` and the initial Caddy config to `/opt/docklet`
   and writes `/opt/docklet/.env` (mode 600).
4. Pulls `ghcr.io/monirujjaman-dev/docklet` and `caddy`, starts both, and
   waits for the health check.

Re-running the same command **upgrades** to the newest release, after an
automatic backup. Data (`docklet-data`, `caddy-data` volumes) and `.env` are
kept. See [upgrade.md](upgrade.md).

## Options

Pass them after `sudo`:

```sh
curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh \
  | sudo DOCKLET_PORT=3100 DOCKLET_VERSION=1.0.0 sh
```

| Variable | Default | Meaning |
|---|---|---|
| `DOCKLET_VERSION` | latest release | Image tag: a release (`1.0.0`), or `main` for the development branch |
| `DOCKLET_DIR` | `/opt/docklet` | Where `compose.yaml` and `.env` live |
| `DOCKLET_PORT` | `3000` | Dashboard port |
| `DOCKLET_HTTP_PORT` / `DOCKLET_HTTPS_PORT` | `80` / `443` | Caddy ports |
| `DOCKLET_SKIP_PORT_CHECK` | unset | `1` installs even if a port looks busy |
| `DOCKLET_SKIP_BACKUP` | unset | `1` skips the pre-upgrade backup |

Values you pass are saved to `.env`, so later upgrades keep them.

## On a server that already runs Dokploy (or Coolify, nginx…)

Those platforms already hold ports 80/443, and the installer will stop with
a message saying so. You have two options:

- **Replace it:** uninstall or stop the other platform first, following its
  own documentation (check afterwards with `docker ps` and `docker service ls`
  that nothing publishes ports 80/443), then run the installer.
- **Side by side, for testing:** use other ports. Caddy then can't get
  Let's Encrypt certificates (they need ports 80/443), so use it for trying
  the dashboard and deployments only:

  ```sh
  curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh \
    | sudo DOCKLET_HTTP_PORT=8080 DOCKLET_HTTPS_PORT=8443 DOCKLET_PORT=3100 sh
  ```

## After installing

1. Create the admin account and enable 2FA (Account).
2. **Settings → Panel domain**: create a DNS A/AAAA record (e.g.
   `panel.example.com`) for the server and save it. Docklet checks that
   HTTPS works end to end.
3. Close the direct port:

   ```sh
   cd /opt/docklet
   sed -i 's/^DOCKLET_BIND_IP=.*/DOCKLET_BIND_IP=127.0.0.1/' .env
   docker compose up -d
   ```

## Maintenance

```sh
cd /opt/docklet
docker compose logs -f docklet                                   # logs
docker compose exec docklet docklet audit verify                 # audit chain check
docker compose exec docklet docklet admin reset-password -username admin
docker compose exec docklet docklet admin clear-panel-domain     # then: docker compose restart docklet
```

Back up Docklet itself from **Settings → Back up Docklet**, or with
`docklet backup` ([backup.md](backup.md)). The backup holds `docklet.db`
**and** `master.key`: without the key, encrypted env vars and 2FA secrets
can't be recovered. If something goes wrong, see
[troubleshooting.md](troubleshooting.md).

## Uninstall

Delete your apps in the dashboard first (or remove their containers with
`docker ps -a --filter label=docklet.managed=true`), then:

```sh
cd /opt/docklet && docker compose down -v && cd / && rm -rf /opt/docklet
```
