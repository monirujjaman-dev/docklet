# Benchmarks

Docklet's development plan sets **Global Budgets** that every release has to
meet. `scripts/bench` measures them; run it yourself:

```sh
make build
go run ./scripts/bench -binary bin/docklet                         # budgets on a throwaway instance
sudo go run ./scripts/bench -binary bin/docklet \
     -containers docklet,docklet-caddy                             # + your installed containers
```

It starts its own Docklet (temporary data directory, Docker and Caddy
integration off) and measures:

- cold start;
- p50, p95 and p99 latency of four non-build endpoints (400 requests each, 4 concurrent);
- the gzipped size of the initial dashboard JavaScript;
- idle RSS.

With `-containers` it also adds up `VmRSS` of every process in the named
containers. `-compare 'Name=c1,c2,…'` does the same for other containers.
The exit status is non-zero if any budget fails.

## Docklet v1.0.0 — 2026-11-21

Measured on a 2 vCPU / 8 GB Linux VM with Docker 29. The installed figures are
for Docklet managing 1 app, 1 PostgreSQL database and a cron job, after 30 s idle.

| Budget | Limit | Measured | |
|---|---|---|---|
| Binary size (linux/amd64) | ≤ 40 MB | **12.8 MB** | ✅ |
| Cold start (`docklet serve` → healthy) | ≤ 200 ms | **64 ms** | ✅ |
| API p95 (`/api/auth/me`, `/api/projects`, `/api/audit`, `/api/settings/panel`) | ≤ 50 ms | **2.4 ms** (p99 3.9 ms) | ✅ |
| Dashboard JS, gzipped, initial | ≤ 150 KB | **71 KB** | ✅ |
| Docklet idle RSS | ≤ 40 MB | **18.6 MB** | ✅ |
| Docklet + Caddy idle RSS | ≤ 80 MB | **63.5 MB** (Caddy 44.9 MB) | ✅ |

Other results from the same run:

| Check | Result |
|---|---|
| Fresh install → first app (`nginx:alpine` with a domain) running | 4.3 s, with images already pulled. Pulls depend on bandwidth; on a 1 GB VPS this stays well under the 5 minute target |
| Upgrade (v0.7.0 → v1.0.0 with the installer) | 2.9 s, including a pre-upgrade backup. 0 of 51 requests failed through Caddy and 0 of 51 directly to the app. The app container was not recreated; projects, apps, the database's data, the domain route, the cron job and the audit chain were unchanged |

## Compared with Dokploy

Dokploy v0.31.4 (`dokploy/dokploy:latest`, 2026-11-21) was started on the same
VM, following the steps of its installer: `dokploy`, `postgres:16` and
`traefik:v3.7.19`, with its secrets, mounts and environment.

The test VM's kernel has no VXLAN support, so Swarm's overlay network
couldn't carry traffic. The three containers therefore ran on a bridge
network, with Swarm still initialised. The processes are the same as a normal
install, but a real install also runs the Swarm overlay and may differ slightly.
Neither platform had apps deployed by itself (Docklet's numbers above include
the one app it manages). Both were measured after 2 minutes idle.

| | Docklet + Caddy | Dokploy + Postgres + Traefik |
|---|---|---|
| Idle RSS, sum of process `VmRSS` | **63.5 MB** | 753.7 MB (dokploy 504.9, postgres 154.2, traefik 94.6) |
| Idle memory, `docker stats` (cgroup) | **21 MiB** (7.9 + 13.3) | 525 MiB (455.8 + 51.2 + 18.3) |
| Start → dashboard answering | 64 ms (binary); installer 2.2 s | ≈ 120 s (waits for Postgres, then runs migrations) |
| Services required | 2 containers | Swarm + 3 containers (the `dokploy/dokploy` image alone is 4.3 GB) |

`VmRSS` counts shared pages once per process, so it overstates multi-process
programs such as PostgreSQL. `docker stats` reports cgroup memory without the
page cache. Both measures show Docklet's control plane using about 4% of
Dokploy's memory. On a 1 GB VPS that difference is the room left for your apps.
