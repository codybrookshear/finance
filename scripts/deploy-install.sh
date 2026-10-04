#!/usr/bin/env bash
# deploy-install.sh: install or update the app on the droplet. Run by
# scripts/deploy.sh with sudo; SRC is the private temp dir it filled:
#   images.tar.gz, compose.yaml, compose.prod.yaml, 01-roles.sh,
#   finance-sync.{service,timer}, finance-access-keys.{service,timer},
#   finance-web.tmpfiles, secrets/<name> (deleted here)
#
# Layout:
#   /opt/finance              compose files and .env (no secrets)
#   /etc/finance/secrets      one file per secret: directory 0700 root, files
#                             0444 so the containers' non-root users can read
#                             them through their bind mounts
#   /etc/finance/access       Cloudflare Access signing keys (public), for web
#   /run/finance-web          web's socket, which cloudflared connects to
#   Docker volume finance_pgdata   the database

set -euo pipefail

SRC="${1:?usage: deploy-install.sh <dir> <version>}"
VERSION="${2:?usage: deploy-install.sh <dir> <version>}"
APP=/opt/finance
SECRETS=/etc/finance/secrets
SOCKET=/run/finance-web/web.sock

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
[[ "$VERSION" =~ ^[0-9a-f]{7,40}$ ]] || die "bad version: $VERSION"
command -v docker >/dev/null || die "docker isn't installed; run bootstrap-droplet.sh first"

# Secrets first: out of the temp dir, whatever happens next.
install -d -m 0700 /etc/finance "$SECRETS"
for f in "$SRC"/secrets/*; do
  name="$(basename "$f")"
  [[ "$name" =~ ^[a-z][a-z0-9_]*$ ]] || die "unexpected secret name: $name"
  [[ -s "$f" ]] || die "secret $name is empty"
  install -m 0444 "$f" "$SECRETS/$name"
done
rm -rf "$SRC/secrets"

echo "Loading images..."
gunzip -c "$SRC/images.tar.gz" | docker load -q

install -m 0644 "$SRC/compose.yaml" "$SRC/compose.prod.yaml" "$APP/"
install -d -m 0755 "$APP/db" "$APP/db/init"
install -m 0755 "$SRC/01-roles.sh" "$APP/db/init/"
cat >"$APP/.env" <<EOF
COMPOSE_PROJECT_NAME=finance
COMPOSE_FILE=compose.yaml:compose.prod.yaml
FINANCE_SECRETS_DIR=$SECRETS
FINANCE_VERSION=$VERSION
EOF
chmod 0644 "$APP/.env"

# Web prerequisites: the socket directory (now, and at every boot) and the
# Access signing keys, which the web app needs before it will start.
install -m 0644 "$SRC/finance-web.tmpfiles" /etc/tmpfiles.d/finance-web.conf
systemd-tmpfiles --create /etc/tmpfiles.d/finance-web.conf
install -d -m 0755 /etc/finance/access
install -m 0644 "$SRC/finance-sync.service" "$SRC/finance-sync.timer" \
  "$SRC/finance-access-keys.service" "$SRC/finance-access-keys.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl start finance-access-keys.service || die "couldn't fetch the Access signing keys"
# enable --now leaves an already-running timer on its old schedule; restart it.
systemctl enable --quiet finance-access-keys.timer
systemctl restart finance-access-keys.timer

cd "$APP"
docker compose config --quiet
echo "Starting Postgres and running migrations..."
docker compose up --detach --wait postgres
docker compose run --rm migrate

echo "Running a sync now..."
if ! systemctl start finance-sync.service; then
  journalctl -u finance-sync -n 30 --no-pager -o cat
  die "the sync failed; see above"
fi
journalctl -u finance-sync -n 4 --no-pager -o cat | grep -E 'sync complete|categorized' || true
systemctl enable --quiet finance-sync.timer
systemctl restart finance-sync.timer

echo "Starting the web app..."
docker compose up --detach web
# Without an Access login it must answer 403: proof it's up and checking.
code=""
for _ in $(seq 1 20); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket "$SOCKET" \
    http://finance.brookshear.party/transactions || true)"
  [[ "$code" == 403 ]] && break
  sleep 1
done
if [[ "$code" != 403 ]]; then
  docker compose logs --tail 30 web
  die "the web app isn't answering as expected (got '$code', want 403); see above"
fi
echo "Web app is up on $SOCKET and refuses requests without an Access login."

# Keep the current images and the previous version's, for rolling back.
keep="$(docker images --format '{{.Tag}}' finance-sync | grep -vx "$VERSION" | head -1 || true)"
for repo in finance-migrate finance-sync finance-web; do
  { docker images --format '{{.Repository}}:{{.Tag}}' "$repo" |
      grep -v -e ":$VERSION\$" ${keep:+-e ":$keep\$"} | xargs -r docker rmi -f >/dev/null; } || true
done

echo
echo "Deployed $VERSION."
systemctl list-timers finance-sync.timer --no-pager | head -2
