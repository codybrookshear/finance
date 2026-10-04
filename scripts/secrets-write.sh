#!/usr/bin/env bash
# secrets-write.sh: write the secrets an env file refers to into a directory,
# one file per secret, straight from 1Password (values never pass through the
# shell). Compose mounts them into containers as /run/secrets/<name>.
#
#   scripts/secrets-write.sh env/dev.env "$XDG_RUNTIME_DIR/finance-dev-secrets"
#
# Each KEY=op://... line becomes <dir>/<key in lowercase>, e.g.
# SIMPLEFIN_ACCESS_URL → simplefin_access_url; other lines are ignored.
# The directory is 0700, so other users here can't get in. The files are 0444
# because bind mounts keep host permissions, and the containers' non-root
# users must be able to read them.

set -euo pipefail

ENV_FILE="${1:?usage: scripts/secrets-write.sh <env file> <output dir>}"
DIR="${2:?usage: scripts/secrets-write.sh <env file> <output dir>}"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ -f "$ENV_FILE" ]] || die "no such env file: $ENV_FILE"
command -v op >/dev/null || die "1Password CLI (op) not found"
op whoami >/dev/null 2>&1 || die "sign in to the 1Password CLI first: eval \"\$(op signin)\""

umask 077
mkdir -p "$DIR"
chmod 700 "$DIR"

n=0
while IFS='=' read -r key ref; do
  [[ "$ref" == op://* ]] || continue
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || die "unexpected key in $ENV_FILE: $key"
  name="$(printf '%s' "$key" | tr '[:upper:]' '[:lower:]')"
  rm -f "$DIR/$name"
  op read --no-newline --force --file-mode 0400 --out-file "$DIR/$name" "$ref" >/dev/null \
    || die "couldn't read $ref from 1Password"
  chmod 0444 "$DIR/$name"   # after writing: umask 077 would mask a 0444 --file-mode
  n=$((n + 1))
done <"$ENV_FILE"
((n > 0)) || die "no op:// references in $ENV_FILE"
echo "Wrote $n secrets from 1Password to $DIR."
