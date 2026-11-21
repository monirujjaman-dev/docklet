# Managing the server

The **Server** page manages the machine Docklet runs on:

- host name and time zone
- swap
- disk cleanup
- reboot
- listening ports
- processes
- systemd services

## Requirements

**Changes** (host name, time zone, swap, services, process signals, reboot,
host-side cleanup) need **Ubuntu or Debian with systemd**. On other hosts the
page is read-only: overview, ports, processes and Docker cleanup still work.

## How it works

Docklet runs in a container, so it reaches the host through a **helper**:
for each action it starts a short-lived privileged container from its own
image. The helper enters the host's namespaces with `nsenter -t 1`, runs one
command and is removed. Nothing runs on the host between actions, and nothing
extra is installed. See ADR-002 in the project for the reasoning.

- Commands are fixed argument lists, or fixed scripts written in Docklet;
  values you type reach them only as environment variables, never as shell
  code.
- Every change is recorded in the audit log with its before and after values.
- A binary install (`deploy/docklet.service`, running as root) runs the same
  commands directly.

> Anyone who can sign in to Docklet can control the server as root. That was
> already true through Docker. Use a strong password, 2FA and HTTPS
> ([security.md](security.md)).

## Features

| Feature | What it does |
|---|---|
| Host name | `hostnamectl set-hostname`, and keeps the `127.0.1.1` line in `/etc/hosts` in step |
| Time zone | `timedatectl set-timezone`, searchable list. Cron jobs keep their own time zones |
| Swap | Creates, resizes or removes `/swapfile` (256 MB – 64 GB; refuses unless 1 GB of disk stays free) and adds it to `/etc/fstab`. Swappiness is set now and in `/etc/sysctl.d/99-docklet.conf`. Recommended on 1–2 GB servers, with swappiness 10 |
| Disk cleanup | Preview with sizes, then remove what you tick: unused images, build cache, stopped containers not managed by Docklet, old journal logs (keeps 100 MB) and the apt cache. **Unused volumes** are listed, but only the ones you tick are deleted |
| Reboot | Needs your password and the host name typed in. Runs in 5 seconds (`systemd-run`); the page waits until the server answers again. Apps, databases, Caddy and Docklet come back through their restart policies |
| Listening ports | `ss -tulpn`, with the owning process and, for published ports, the container and Docklet app or database. Ports on 127.0.0.1 are local only |
| Processes | Top 100 by CPU, with Stop (TERM) and Kill (KILL) |
| Services | systemd services with state and boot setting: start, stop, restart, enable, disable, and the last 200 log lines |

## What is protected

- **Disk cleanup never removes:**
  - images of current or previous deployments (rollback targets);
  - images used by any container;
  - Docklet's and Caddy's images;
  - containers or volumes created by Docklet (apps, databases, cron, Docklet itself).
- **Processes:** PID 1, kernel threads, `dockerd`, `containerd`, `docker-proxy`, `sshd`, Docklet and Caddy can't be signalled.
- **Services:**
  - `docker`, `containerd`, `dbus`, `systemd-*` and `docklet*` can't be stopped, restarted or disabled;
  - `ssh`/`sshd` can be restarted but not stopped or disabled. Changing SSH safely is part of M11.
- **Input validation:** host names must follow RFC 1123, time zones must be in the host's list, and unit names must be valid `*.service` names.

## Troubleshooting

| Symptom | Cause |
|---|---|
| "server management is unavailable: find Docklet's own container" | Docklet can't inspect its own container. Set `DOCKLET_HELPER_IMAGE` to its image (e.g. `ghcr.io/monirujjaman-dev/docklet:1.2.0`) |
| "nsenter: can't open '/proc/1/ns/…': Permission denied" | The machine forbids entering PID 1's namespaces (some sandboxes do). On a normal VPS this doesn't happen. `DOCKLET_HOST_PID` can name another process in the host's namespaces |
| "swapon failed: Invalid argument" | The root filesystem can't hold a swap file (e.g. overlayfs, or btrfs without nocow) |
