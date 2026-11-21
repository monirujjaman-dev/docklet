#!/bin/sh
# Docklet installer: runs Docklet and Caddy as containers on a Linux server.
#
#   curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh | sudo sh
#
# Re-running it upgrades to the latest image and keeps your data and settings.
#
# Environment overrides (put them after "sudo", e.g. "| sudo DOCKLET_PORT=3100 sh"):
#   DOCKLET_VERSION     image tag to install: a release such as 1.0.0, or "main"
#                       for the development branch (default: the latest release)
#   DOCKLET_DIR         install directory (default: /opt/docklet)
#   DOCKLET_PORT        dashboard port (default: 3000)
#   DOCKLET_HTTP_PORT   Caddy HTTP port (default: 80)
#   DOCKLET_HTTPS_PORT  Caddy HTTPS port (default: 443)
#   DOCKLET_SKIP_PORT_CHECK=1  install even if a port is already in use
set -eu

REPO="${DOCKLET_REPO:-monirujjaman-dev/docklet}"
REQUESTED_VERSION="${DOCKLET_VERSION:-}"
DIR="${DOCKLET_DIR:-/opt/docklet}"
PORT="${DOCKLET_PORT:-3000}"
HTTP_PORT="${DOCKLET_HTTP_PORT:-80}"
HTTPS_PORT="${DOCKLET_HTTPS_PORT:-443}"
RAW="https://raw.githubusercontent.com/${REPO}/${DOCKLET_BRANCH:-main}"
SUBNET="172.30.0.0/24"

if [ -t 1 ]; then
	BOLD="$(printf '\033[1m')"; RED="$(printf '\033[31m')"; GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; RESET="$(printf '\033[0m')"
else
	BOLD=""; RED=""; GREEN=""; YELLOW=""; RESET=""
fi
info() { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*"; }
warn() { printf '%sWarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die() { printf '%sError:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

# --- Preconditions ------------------------------------------------------------

[ "$(uname -s)" = "Linux" ] || die "this installer supports Linux only (on Windows/macOS use Docker Desktop: see docs/docker-desktop.md)"
[ "$(id -u)" -eq 0 ] || die "please run as root: curl -sSL ${RAW}/install.sh | sudo sh"

case "$(uname -m)" in
	x86_64 | amd64) ;;
	aarch64 | arm64) ;;
	*) die "unsupported CPU architecture $(uname -m) (supported: x86_64, arm64)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"

# --- Docker -------------------------------------------------------------------

if ! command -v docker >/dev/null 2>&1; then
	info "Docker not found; installing it with get.docker.com"
	curl -fsSL https://get.docker.com | sh
fi
docker info >/dev/null 2>&1 || {
	command -v systemctl >/dev/null 2>&1 && systemctl enable --now docker >/dev/null 2>&1 || true
	docker info >/dev/null 2>&1 || die "the Docker daemon is not running"
}
docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is missing (install the docker-compose-plugin package)"

api="$(docker version --format '{{.Server.APIVersion}}' 2>/dev/null || echo 0)"
major="${api%%.*}"; minor="${api#*.}"; minor="${minor%%.*}"
if [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt 44 ]; }; then
	die "Docker Engine API $api is too old; Docker 25 or newer is required"
fi

# --- Conflicts ----------------------------------------------------------------

existing=0
docker ps -a --format '{{.Names}}' | grep -qx docklet && existing=1

# A TCP port is in use if any IPv4/IPv6 socket is LISTENing on it (state 0A).
# Reading /proc works on every Linux, even without ss/netstat installed.
port_in_use() {
	hex="$(printf '%04X' "$1")"
	cat /proc/net/tcp /proc/net/tcp6 2>/dev/null |
		awk -v p="$hex" 'NR > 1 && $4 == "0A" { n = split($2, a, ":"); if (toupper(a[n]) == p) found = 1 } END { exit !found }'
}

if [ "$existing" -eq 0 ] && [ "${DOCKLET_SKIP_PORT_CHECK:-0}" != "1" ]; then
	busy=""
	for p in "$HTTP_PORT" "$HTTPS_PORT" "$PORT"; do
		port_in_use "$p" && busy="$busy $p"
	done
	if [ -n "$busy" ]; then
		printf '%sError:%s port(s)%s already in use on this server.\n' "$RED" "$RESET" "$busy" >&2
		command -v ss >/dev/null 2>&1 && for p in $busy; do ss -Hltnp "sport = :$p" 2>/dev/null | sed 's/^/    /' >&2; done
		cat >&2 <<EOF

Docklet's Caddy needs ports ${HTTP_PORT}/${HTTPS_PORT} for HTTPS. If another platform
(Dokploy/Traefik, Coolify, nginx…) already uses them, either stop it first or
install on other ports, for example:

    curl -sSL ${RAW}/install.sh | sudo DOCKLET_HTTP_PORT=8080 DOCKLET_HTTPS_PORT=8443 DOCKLET_PORT=3100 sh

(Automatic Let's Encrypt certificates only work when Caddy owns ports 80/443.)
EOF
		exit 1
	fi
fi

for net in $(docker network ls -q); do
	name="$(docker network inspect -f '{{.Name}}' "$net")"
	[ "$name" = "docklet" ] && continue
	if docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' "$net" | grep -qw "$SUBNET"; then
		die "Docker network '$name' already uses $SUBNET, which Docklet needs"
	fi
done

# --- Version ------------------------------------------------------------------

# The newest published release as an image tag ("v1.2.3" -> "1.2.3"), or
# nothing if there is none yet or GitHub can't be reached.
latest_release() {
	curl -fsSL --max-time 15 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name": *"v\{0,1\}\([0-9][0-9.]*\)".*/\1/p' | head -n 1
}
is_release() { echo "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; }

CURRENT_VERSION=""
[ -f "$DIR/.env" ] && CURRENT_VERSION="$(sed -n 's/^DOCKLET_VERSION=//p' "$DIR/.env")"
if [ -n "$REQUESTED_VERSION" ]; then
	VERSION="$REQUESTED_VERSION"
elif [ -n "$CURRENT_VERSION" ] && ! is_release "$CURRENT_VERSION"; then
	VERSION="$CURRENT_VERSION" # a channel such as "main" stays on that channel
else
	VERSION="$(latest_release)"
	if [ -z "$VERSION" ]; then
		VERSION="${CURRENT_VERSION:-main}"
		warn "could not find a published release; using '$VERSION'"
	fi
fi
DOCKLET_VERSION="$VERSION"

# --- Files --------------------------------------------------------------------

if [ "$existing" -eq 1 ]; then
	info "Upgrading Docklet in $DIR: ${CURRENT_VERSION:-unknown} -> $VERSION"
else
	info "Installing Docklet ($VERSION) into $DIR"
fi
mkdir -p "$DIR/deploy/caddy"
fetch() { # fetch <repo path> <destination>
	if [ -n "${DOCKLET_SOURCE_DIR:-}" ]; then
		cp "$DOCKLET_SOURCE_DIR/$1" "$2"
	else
		curl -fsSL "$RAW/$1" -o "$2.tmp" && mv "$2.tmp" "$2"
	fi
}
fetch compose.yaml "$DIR/compose.yaml"
fetch deploy/caddy/caddy.json "$DIR/deploy/caddy/caddy.json"

if [ ! -f "$DIR/.env" ]; then
	cat >"$DIR/.env" <<EOF
# Docklet settings; edit and run: cd $DIR && docker compose up -d
DOCKLET_VERSION=$VERSION
DOCKLET_PORT=$PORT
DOCKLET_HTTP_PORT=$HTTP_PORT
DOCKLET_HTTPS_PORT=$HTTPS_PORT
# After the panel domain works (Settings), set this to 127.0.0.1 so the
# dashboard is reachable only through Caddy:
DOCKLET_BIND_IP=0.0.0.0
EOF
	chmod 600 "$DIR/.env"
fi

# Values passed explicitly to this run are saved to .env; otherwise .env wins.
set_env() { # set_env KEY VALUE
	if grep -q "^$1=" "$DIR/.env"; then
		sed -i "s|^$1=.*|$1=$2|" "$DIR/.env"
	else
		printf '%s=%s\n' "$1" "$2" >>"$DIR/.env"
	fi
}
for key in DOCKLET_VERSION DOCKLET_PORT DOCKLET_HTTP_PORT DOCKLET_HTTPS_PORT DOCKLET_BIND_IP; do
	eval "val=\${$key:-}"
	[ -n "$val" ] && set_env "$key" "$val"
	unset "$key" # let compose read .env, not this shell
done
PORT="$(sed -n 's/^DOCKLET_PORT=//p' "$DIR/.env")"; PORT="${PORT:-3000}"

# --- Start --------------------------------------------------------------------

cd "$DIR"
if [ "${DOCKLET_NO_PULL:-0}" != "1" ]; then
	info "Pulling images"
	docker compose pull --quiet || die "could not pull images (is ghcr.io reachable and the package public?)"
fi

# Before upgrading, stop Docklet briefly and back up its database and key
# with the new image (sites keep running: Caddy holds their routes). The
# backup stays in the data volume, next to the key it contains.
BACKUP_NOTE=""
if [ "$existing" -eq 1 ] && [ "${DOCKLET_SKIP_BACKUP:-0}" != "1" ]; then
	stamp="$(date -u +%Y%m%d-%H%M%S)"
	file="/var/lib/docklet/self-backups/pre-upgrade-${stamp}.tar.gz"
	image="$(docker compose config --images | grep -v '^caddy' | head -n 1)"
	volume="$(docker volume ls -q --filter label=com.docker.compose.project=docklet --filter label=com.docker.compose.volume=docklet-data | head -n 1)"
	[ -n "$image" ] && [ -n "$volume" ] || die "could not find the Docklet image or data volume"
	backup() { docker run --rm --network none -v "$volume:/var/lib/docklet" "$@"; }
	info "Backing up Docklet's database before the upgrade"
	docker compose stop docklet >/dev/null 2>&1 || true
	if backup "$image" backup -no-encrypt -o "$file" >/dev/null; then
		BACKUP_NOTE="Pre-upgrade backup: $file (in the docklet-data volume)"
		# Keep the three newest pre-upgrade backups.
		backup --entrypoint sh "$image" -c \
			'ls -1t /var/lib/docklet/self-backups/pre-upgrade-*.tar.gz 2>/dev/null | tail -n +4 | xargs -r rm -f' >/dev/null 2>&1 || true
	else
		docker compose start docklet >/dev/null 2>&1 || true
		die "the pre-upgrade backup failed, so nothing was changed (set DOCKLET_SKIP_BACKUP=1 to upgrade anyway)"
	fi
fi
info "Starting containers"
docker compose up -d --no-build --remove-orphans

printf 'Waiting for Docklet'
i=0
until curl -fsS "http://127.0.0.1:${PORT}/api/health" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -gt 60 ] && { echo; docker compose logs --tail 30 docklet >&2; die "Docklet did not become healthy"; }
	printf '.'; sleep 1
done
echo

ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$ip" ] || ip="<server-ip>"
ver="$(docker compose exec -T docklet docklet version 2>/dev/null || true)"

cat <<EOF

${GREEN}${BOLD}Docklet is running.${RESET} ${ver}

  Dashboard:  http://${ip}:${PORT}   (create the admin account there)
  Files:      ${DIR}   (compose.yaml, .env)

Next steps:
  1. Open the dashboard and create the admin account ${BOLD}right away${RESET}:
     until then anyone who can reach port ${PORT} can claim it.
  2. Settings → Panel domain: point a DNS record at this server and save.
  3. Then set DOCKLET_BIND_IP=127.0.0.1 in ${DIR}/.env and run
     'cd ${DIR} && docker compose up -d' so port ${PORT} is no longer public.

Upgrade: re-run this installer.   Logs: cd ${DIR} && docker compose logs -f docklet
EOF
if [ -n "$BACKUP_NOTE" ]; then
	cat <<EOF

${BACKUP_NOTE}
To roll back: set DOCKLET_VERSION=${CURRENT_VERSION} in ${DIR}/.env, run 'docker compose up -d',
and if Docklet then fails to start, restore that backup (docs/upgrade.md).
EOF
fi
