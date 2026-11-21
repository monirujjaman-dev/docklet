# Security

## Trust model

Docklet controls the Docker Engine, and **access to the Docker socket is
equivalent to root on the host**. Anyone who can log in to the dashboard can
run any container, so treat the admin account like root:

- Create the admin account right after installing. Until it exists, anyone who
  reaches port 3000 can claim it.
- Turn on two-factor authentication (Account).
- Set a panel domain (HTTPS), then bind port 3000 to `127.0.0.1`
  ([install.md](install.md#after-installing)).

Only the Docklet container mounts the Docker socket. App, worker, cron and
database containers never get it, and Docklet never mounts host paths into
them.

## What protects what

| Area | Measure |
|---|---|
| Passwords | argon2id; login rate limits and a temporary lockout after 5 failures |
| Sessions | Random tokens, stored hashed. Cookies are `HttpOnly`, `SameSite=Strict`, and `Secure` over HTTPS. Idle and absolute expiry; changing the password signs out other sessions |
| CSRF | Per-session token required on every mutating request, plus a same-origin check |
| Secrets | Env vars, database passwords, deploy keys and TOTP secrets are encrypted with XChaCha20-Poly1305 under `master.key` (mode 0600), bound to their owner with associated data |
| Audit | Every admin action is appended to a hash-chained log. Triggers block UPDATE and DELETE; `docklet audit verify` finds tampering |
| HTTP headers | Strict CSP (`script-src 'self'`, `object-src 'none'`, `frame-ancestors 'none'`), `X-Frame-Options: DENY`, `nosniff`, COOP/CORP same-origin, a restrictive Permissions-Policy, and HSTS over HTTPS |
| Public endpoints | Only health, version (the release name only), setup (until an admin exists), login, the panel-domain probe and webhooks (HMAC or token, constant-time comparison) |
| Caddy admin API | Listens only on `172.30.0.2:2019`, on the internal `docklet` network. App containers are on per-project networks and can't reach it (Docker isolates bridge networks) |
| Databases | Private to their project's network by default; a public port is an explicit, audited toggle |
| Git builds | Only `https`, `http` and `ssh` URLs, also for redirects (`GIT_ALLOW_PROTOCOL`). No credentials in URLs; branch and paths are validated. Build context and Dockerfile are resolved with symlinks and must stay inside the checkout |
| Inputs to Docker and Caddy | Names, image references, env keys, domains, ports, limits and commands are validated. Commands run without a shell, so there is no interpolation |
| Backups | Self-backups contain the master key. Downloads need your password again and are always encrypted (argon2id + XChaCha20-Poly1305, with chunks authenticated so truncation or reordering is detected) and audited |
| Upgrades | `docklet upgrade` verifies the SHA-256 of the release binary against the release's `checksums.txt`. The installer backs up before upgrading |
| Update check | Only when an admin opens Settings → Updates; it fetches the public release list and sends nothing about the server |

## Reporting a vulnerability

Please report it privately through GitHub's **Security → Report a
vulnerability** on the repository, not in a public issue.
