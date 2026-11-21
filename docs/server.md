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

## Security tab

The Security tab manages SSH, the ufw firewall and Fail2Ban. All of it is built so that a mistake can't lock you out of the server.

### Confirm or undo

Changing SSH settings and turning the firewall on are **confirm-or-undo** changes:

1. Docklet applies the change and, in the same step, schedules a job **on the server itself** (`systemd-run`, unit `docklet-revert-*`) that puts the old settings back after 5 minutes.
2. The page shows a banner with a countdown. For SSH, open a **new** SSH session with the new settings and keep the old one open.
3. If the new settings work, click **It works, keep it**. If they don't, click **Undo now**, or do nothing: the server undoes the change by itself, even if Docklet or your browser is gone.

Only one such change can be pending at a time. Both steps need your password.

### SSH

| Setting | How it's applied |
|---|---|
| Port | A new port is opened **next to** the old one (and allowed in ufw when ufw is on). On confirm the old port is closed in sshd and in ufw. Ports used by Docklet, Caddy or a public database are refused |
| Root login | `PermitRootLogin`: SSH key only (`prohibit-password`), key or password (`yes`), or not allowed (`no`) |
| Password login | `PasswordAuthentication` (and `KbdInteractiveAuthentication`). Turning it off is refused while no SSH key exists for root or a sudo user |

The settings are written to `/etc/ssh/sshd_config.d/00-docklet.conf`. It sorts first, so it wins over `50-cloud-init.conf` and the main `sshd_config` (sshd uses the first value it reads). Every change is checked with `sshd -t` before sshd is restarted; a config sshd rejects is never applied. Ubuntu 22.10+ starts sshd from `ssh.socket`; Docklet runs `daemon-reload` and restarts the socket so the new port takes effect.

**SSH keys** lists the `authorized_keys` of root and sudo users with their fingerprints. You can add a key (OpenSSH format: `ssh-ed25519 AAAA… comment`) for root or a sudo user, and remove one. Removing the last key while password login is off is refused.

### Firewall (ufw)

- **Install** runs `apt-get install ufw` if it's missing.
- **Turn on** sets incoming to deny and outgoing to allow, after allowing these ports (they are always kept open):
  - the SSH port(s);
  - HTTP and HTTPS (TCP 80/443 and UDP 443 for HTTP/3, or `DOCKLET_HTTP_PORT` / `DOCKLET_HTTPS_PORT`);
  - the panel port (`DOCKLET_PORT`, default 3000).
- **Rules:** allow, deny, reject or limit a port or range (`6000:6100`), TCP/UDP/any, from anywhere or one IP/CIDR, with a comment.
  - Deny and reject rules are inserted at the top, so they win over broader allows.
  - A deny or reject that covers a protected port is refused, and so is deleting the last rule that lets a protected port in.
  - A delete only goes through if the rule still reads the same as when you loaded the list (rule numbers shift when rules change).
- **Default policies** for incoming and outgoing traffic.

> **Docker-published ports skip ufw.** Docker sends traffic for published ports through the `FORWARD` chain, which ufw doesn't filter. That covers Caddy (80/443), the panel (3000) and public database ports. To limit who can reach a public database, use its page (below).

### Public database allowlist

On a database's page, after you publish a port, **Allowed sources** takes IPv4 addresses or CIDRs (empty = anyone).

- Docklet keeps the rules in its own iptables chain `DOCKLET-USER`, jumped from Docker's `DOCKER-USER`.
- Rules match on conntrack's original destination port and state `DNAT`, so only traffic that came in through the published port is filtered. Containers on the server still reach the database directly.
- The rules are re-applied at startup and every 5 minutes, because a reboot or a Docker restart can drop them. They follow a change of the public port and are removed when the database is deleted.
- IPv6 sources are not supported yet.

### Fail2Ban

- **Install** runs `apt-get install fail2ban python3-systemd`.
- **Save** writes `/etc/fail2ban/jail.d/docklet.local` with an `sshd` jail:
  - `backend = systemd`, on the current SSH port(s);
  - failures before a ban, the time window, and the ban length (`-1` = forever);
  - addresses that are never banned (`ignoreip`; localhost is always included);
  - the ban action `nftables-multiport`, or `iptables-multiport` when `nft` is missing.
- The config is checked with `fail2ban-client -t`, then fail2ban is restarted.
- The page lists banned addresses with an **unban** button.

Change the SSH port first, then save the Fail2Ban settings again so the jail watches the new port.

## Terminal

**Terminal** opens a shell in the browser (xterm.js, loaded only when you open the page) over a WebSocket.

| Target | How |
|---|---|
| Server | A root login shell (`bash -l`, or `sh`) on the host, in a helper container that `nsenter`s the host's namespaces with a TTY |
| App or database | `docker exec` of a shell in its running container (also from the **Terminal** button on the app or database page) |

- Colours, `htop`, `vim` and resizing work (`TERM=xterm-256color`). Copy by selecting text; paste with Ctrl/Cmd+V.
- **Recent login:** opening a terminal needs a login (or a re-entered password) from the last 10 minutes.
- **One-time tickets:** the browser first gets a ticket valid for one minute, then opens `/api/terminal/ws`. Connections from other origins are refused.
- **Ending the session:** closing the tab, disconnecting or 30 minutes without input ends the shell.
  - Server: the helper container is stopped, which ends everything started in it.
  - App or database: the shell gets SIGHUP and then SIGKILL, because a closed `docker exec` stream doesn't end the process by itself.
- At most 8 terminals are open at once.
- The start and end of a session (target, duration, why it ended) are written to the audit log. What you type is not recorded.

## Files

**Files** browses the server (`/`) and every local Docker volume, such as a database's data volume or Docklet's own.

- **Browse:** folders first, with size, mode, owner and modified time. Symlinks show their target.
- **Edit:** text files up to 1 MB (UTF-8, no NUL bytes). Saving:
  - keeps the file's mode and owner;
  - writes a temporary file next to it and renames it over the original;
  - is refused if the file changed since you opened it (sha256 check).
- **Upload / download:**
  - uploads up to 1 GB, streamed, with a free-space check; a new file gets the owner of its folder;
  - a file downloads as itself, a folder as a `.tar`.
- **Other changes:** new folder or file, rename, move, delete (asks for confirmation), mode (`chmod`) and owner (`chown`), optionally recursive.
- **Safety:**
  - Every path is resolved on the host with `realpath`. In a volume, `../` and symlinks can't leave the volume's folder; a symlink pointing out of it is refused.
  - Deleting a symlink removes the link, never its target.
  - System folders (`/`, `/etc`, `/usr`, `/var/lib/docker`, …) can't be deleted, moved, renamed or have their mode/owner changed. Nothing under `/proc`, `/sys` or `/dev` can be written.
  - Editing in `/etc`, `/boot`, `/usr` and similar places shows a warning.
- File changes need a login or a re-entered password from the last 10 minutes.
- Every change and every download is audited (path, sizes, sha256).

File contents go through Docker's archive API on a helper container that bind-mounts only the target folder and is never started. So large files never pass through a shell, and memory use stays flat: a 500 MB upload and download used under 10 MB.

## What is protected

- **Disk cleanup never removes:**
  - images of current or previous deployments (rollback targets);
  - images used by any container;
  - Docklet's and Caddy's images;
  - containers or volumes created by Docklet (apps, databases, cron, Docklet itself).
- **Processes:** PID 1, kernel threads, `dockerd`, `containerd`, `docker-proxy`, `sshd`, Docklet and Caddy can't be signalled.
- **Services:**
  - `docker`, `containerd`, `dbus`, `systemd-*` and `docklet*` can't be stopped, restarted or disabled;
  - `ssh`/`sshd` can be restarted but not stopped or disabled; change SSH on the Security tab instead.
- **Input validation:** host names must follow RFC 1123, time zones must be in the host's list, and unit names must be valid `*.service` names.

## Troubleshooting

| Symptom | Cause |
|---|---|
| "nothing to confirm: the host already undid the change" | The 5 minutes ran out (or the server rebooted) before you confirmed. The old settings are back; apply the change again |
| "sshd_config does not include sshd_config.d/*.conf" | An old or customised `sshd_config` without `Include /etc/ssh/sshd_config.d/*.conf`. Add that line at the top |
| "Docker's DOCKER-USER chain is missing" | Docker runs with `"iptables": false`, so it doesn't create the chain. The allowlist needs Docker's iptables integration |
| "server management is unavailable: find Docklet's own container" | Docklet can't inspect its own container. Set `DOCKLET_HELPER_IMAGE` to its image (e.g. `ghcr.io/monirujjaman-dev/docklet:1.2.0`) |
| "nsenter: can't open '/proc/1/ns/…': Permission denied" | The machine forbids entering PID 1's namespaces (some sandboxes do). On a normal VPS this doesn't happen. `DOCKLET_HOST_PID` can name another process in the host's namespaces |
| "swapon failed: Invalid argument" | The root filesystem can't hold a swap file (e.g. overlayfs, or btrfs without nocow) |

## Testing

The security features are tested against a privileged systemd container that stands in for a VPS (`jrei/systemd-ubuntu:24.04` with `openssh-server`, `ufw`, `fail2ban` and `sshpass` installed, a root password and a sudo user `deploy`), with SSH logins from a second container. See `TestRealSecurity` in `internal/host/security_real_test.go`. `TestRealAllowlist` runs on a real Docker host and uses two network namespaces as outside clients. `DOCKLET_CONFIRM_WINDOW_SEC` shortens the 5-minute confirm window for end-to-end tests.
