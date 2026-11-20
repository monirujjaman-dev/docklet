# Running Docklet on Docker Desktop (Windows)

Docklet and Caddy run as two containers next to the apps they manage. This is
the quickest way to try Docklet on a Windows (or macOS) laptop. For a real
server, run the binary with systemd (coming in M8).

## Requirements

- **Docker Desktop** with the **WSL 2** backend and **Linux containers**
  (the default). Docker Engine 25 or newer.
- **Git**, to clone the repository.
- Ports **80**, **443** and **3000** free on Windows. If something (IIS,
  Skype, another web server) already uses them, see [Different ports](#different-ports).

## Start

In PowerShell:

```powershell
git clone https://github.com/monirujjaman-dev/docklet.git
cd docklet
docker compose up -d --build
```

The first build downloads Node, Go and Alpine images and takes a few minutes.
Then open **http://localhost:3000** and create the admin account.

Check status and logs:

```powershell
docker compose ps
docker compose logs -f docklet
```

## Try a deployment

1. **Projects → Create project**, e.g. `demo`.
2. **New app**:
   - Name: `hello`
   - Image: `nginx:alpine` (or `traefik/whoami`)
   - Domains: `hello.localhost`
3. Wait for the deployment to show **succeeded** (Deployments tab).
4. Open **https://hello.localhost**.

Browsers resolve every `*.localhost` name to your machine, so no hosts-file
edits are needed. Caddy serves these names with its **own local certificate
authority**, so the browser shows a certificate warning the first time —
accept it, or trust Caddy's root once (below).

From PowerShell, `curl.exe` needs to be told where `hello.localhost` is:

```powershell
curl.exe -k --resolve hello.localhost:443:127.0.0.1 https://hello.localhost/
```

### Optional: trust Caddy's local CA

```powershell
docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt .\caddy-root.crt
Import-Certificate -FilePath .\caddy-root.crt -CertStoreLocation Cert:\CurrentUser\Root
```

Remove it later from *certmgr.msc → Trusted Root Certification Authorities*.

## What runs where

| Container | Address | Purpose |
|---|---|---|
| `docklet` | `172.30.0.3:3000`, published as `localhost:3000` | Dashboard and API |
| `docklet-caddy` | `172.30.0.2`, ports 80/443 published | Reverse proxy and TLS; admin API on `172.30.0.2:2019` (internal only) |
| `docklet-<project>-<app>-d<N>` | per-project network `docklet-p<id>` | Your apps |

Docklet joins itself and Caddy to every project network
(`DOCKLET_ATTACH_CONTAINERS`) so it can health-check app containers and Caddy
can proxy to them. Data lives in the `docklet-data` and `caddy-data` volumes.

## Different ports

Create a `.env` file next to `compose.yaml`:

```dotenv
DOCKLET_PORT=3100
DOCKLET_HTTP_PORT=8080
DOCKLET_HTTPS_PORT=8443
```

Then `docker compose up -d`. Apps are reached at `https://hello.localhost:8443`
(Caddy's HTTP→HTTPS redirect still points at 443, so use the HTTPS URL directly).

## Panel domain

The panel-domain feature (Settings) needs a public domain and a valid
Let's Encrypt certificate, so it does not work with `*.localhost` on a laptop:
the check will report *failed* and the dashboard stays on `localhost:3000`.
That is expected.

## Update, reset, remove

```powershell
git pull
docker compose up -d --build        # rebuild and restart Docklet

docker compose exec docklet docklet admin reset-password -username admin
docker compose exec docklet docklet audit verify
```

Stop everything (apps keep their containers until you delete them in Docklet):

```powershell
docker compose down
```

Remove Docklet **and all its data** (volumes). Delete your apps in the
dashboard first, or remove their containers afterwards with
`docker ps -a --filter label=docklet.managed=true`:

```powershell
docker compose down -v
```

## Troubleshooting

- **`Bind for 0.0.0.0:80 failed: port is already allocated`** — another
  program uses the port; see [Different ports](#different-ports).
- **Deployment fails with "pull … denied"** — the image is private or the
  name is wrong. Private registries are not supported yet.
- **`https://hello.localhost` doesn't load** — check that the app's status is
  *running* and the domain is listed on its Overview tab, then
  `docker compose logs caddy`.
- **Settings shows "Caddy's admin API is not reachable"** —
  `docker compose ps` should show `docklet-caddy` running; restart it with
  `docker compose restart caddy`. Docklet restores the routes within ~10 s.
