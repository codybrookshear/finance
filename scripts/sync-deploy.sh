#!/usr/bin/env bash
# sync-deploy.sh: deploy the sync stack (Postgres, migrations, SimpleFIN sync
# on a timer) to the droplet. Run on your workstation:
#
#   eval "$(op signin)"
#   scripts/sync-deploy.sh            # deploys HEAD to the `finance` SSH alias
#
# Builds the migrate and sync images from the committed HEAD (not the files on
# disk), ships them over SSH with the compose files and systemd units, sends
# the production secrets from env/prod.env straight from 1Password (never
# written to disk here), and runs scripts/sync-install.sh with sudo there.
# The repo is private, so there's no registry: the droplet needs no GitHub
# credentials.

# $dir (a validated remote temp path) is meant to expand locally, before ssh sends it.
# shellcheck disable=SC2029

set -euo pipefail

DEST="${DEST:-finance}"
ENV_FILE=env/prod.env

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
[[ -z "$(git status --porcelain)" ]] || die "commit your changes first: images are built from HEAD"
version="$(git rev-parse --short=12 HEAD)"
command -v docker >/dev/null || die "docker not found"
command -v op >/dev/null || die "1Password CLI (op) not found"
op whoami >/dev/null 2>&1 || die "sign in to the 1Password CLI first: eval \"\$(op signin)\""

# Every secret must resolve before anything touches the droplet.
secrets=()
while IFS='=' read -r key ref; do
  [[ "$ref" == op://* ]] || continue
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || die "unexpected key in $ENV_FILE: $key"
  op read "$ref" >/dev/null 2>&1 || die "can't read $ref from 1Password (see deploy/finance/README.md)"
  secrets+=("$key=$ref")
done <"$ENV_FILE"
((${#secrets[@]} > 0)) || die "no op:// references in $ENV_FILE"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
echo "Building images for $version..."
for cmd in migrate sync; do
  git archive --format=tar HEAD |
    docker build -q --build-arg CMD="$cmd" \
      --label org.opencontainers.image.revision="$(git rev-parse HEAD)" \
      -t "finance-$cmd:$version" - >/dev/null
done
docker save "finance-migrate:$version" "finance-sync:$version" | gzip >"$work/images.tar.gz"

dir="$(ssh "$DEST" 'mktemp -d')"
[[ "$dir" =~ ^/tmp/tmp\.[A-Za-z0-9]+$ ]] || die "unexpected remote temp dir: $dir"
echo "Copying to the droplet..."
scp -q "$work/images.tar.gz" compose.yaml compose.prod.yaml db/init/01-roles.sh \
  deploy/finance/finance-sync.service deploy/finance/finance-sync.timer \
  scripts/sync-install.sh "$DEST:$dir/"

# Held in memory only; printf is a shell builtin, so they never show up in ps.
ssh "$DEST" "umask 077 && mkdir $dir/secrets"
for s in "${secrets[@]}"; do
  key="${s%%=*}"
  name="$(printf '%s' "$key" | tr '[:upper:]' '[:lower:]')"
  value="$(op read --no-newline "${s#*=}")"
  printf '%s' "$value" | ssh "$DEST" "umask 077 && cat > $dir/secrets/$name"
  unset value
done

rc=0
ssh -t "$DEST" "sudo bash $dir/sync-install.sh $dir $version" || rc=$?
ssh "$DEST" "rm -rf $dir" || true
exit "$rc"
