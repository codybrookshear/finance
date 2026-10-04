#!/usr/bin/env bash
# sync-install.sh: install or update the sync stack on the droplet. Run by
# scripts/sync-deploy.sh with sudo; SRC is the private temp dir it filled:
#   images.tar.gz, compose.yaml, compose.prod.yaml, 01-roles.sh,
#   finance-sync.service, finance-sync.timer, secrets/<name> (deleted here)
#
# Layout:
#   /opt/finance              compose files and .env (no secrets)
#   /etc/finance/secrets      one file per secret: directory 0700 root, files
#                             0444 so the containers' non-root users can read
#                             them through their bind mounts
#   Docker volume finance_pgdata   the database

set -euo pipefail

SRC="${1:?usage: sync-install.sh <dir> <version>}"
VERSION="${2:?usage: sync-install.sh <dir> <version>}"
APP=/opt/finance
SECRETS=/etc/finance/secrets

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

cd "$APP"
docker compose config --quiet
echo "Starting Postgres and running migrations..."
docker compose up --detach --wait postgres
docker compose run --rm migrate

install -m 0644 "$SRC/finance-sync.service" "$SRC/finance-sync.timer" /etc/systemd/system/
systemctl daemon-reload

echo "Running a sync now..."
if ! systemctl start finance-sync.service; then
  journalctl -u finance-sync -n 30 --no-pager -o cat
  die "the sync failed; see above"
fi
journalctl -u finance-sync -n 4 --no-pager -o cat | grep -E 'sync complete|categorized' || true
systemctl enable --now --quiet finance-sync.timer

# Keep the current images and the previous version's, for rolling back.
keep="$(docker images --format '{{.Tag}}' finance-sync | grep -vx "$VERSION" | head -1 || true)"
for repo in finance-migrate finance-sync; do
  { docker images --format '{{.Repository}}:{{.Tag}}' "$repo" |
      grep -v -e ":$VERSION\$" ${keep:+-e ":$keep\$"} | xargs -r docker rmi -f >/dev/null; } || true
done

echo
echo "Deployed $VERSION."
systemctl list-timers finance-sync.timer --no-pager | head -2
