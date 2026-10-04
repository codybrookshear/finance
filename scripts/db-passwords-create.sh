#!/usr/bin/env bash
# db-passwords-create.sh: generate the three Postgres role passwords and store
# them as a new 1Password item, without printing them or putting them in a
# command line.
#
#   scripts/db-passwords-create.sh finance-dev-db    # for make dev
#   scripts/db-passwords-create.sh finance-prod-db   # for the droplet
#
# Fields: owner_password, sync_password, web_password (what env/*.env refer to).
# Hex, so they're safe inside connection URLs.

set -euo pipefail

ITEM="${1:?usage: scripts/db-passwords-create.sh <1Password item title>}"
OP_VAULT="Finance App"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ "$ITEM" =~ ^[a-z0-9-]+$ ]] || die "item title must be lowercase letters, digits and dashes"
command -v op >/dev/null || die "1Password CLI (op) not found"
op whoami >/dev/null 2>&1 || die "sign in to the 1Password CLI first: eval \"\$(op signin)\""
op vault get "$OP_VAULT" >/dev/null 2>&1 || die "1Password vault '$OP_VAULT' not found (or not accessible)"
# op:// references look items up by title and can match an archived item, so
# the title must be unused in the vault AND its Archive.
items="$(op item list --vault "$OP_VAULT" --include-archive --format json)" || die "couldn't list items in '$OP_VAULT'"
if grep -Eq "\"title\": *\"$ITEM\"" <<<"$items"; then
  die "1Password already has an item titled '$ITEM' in '$OP_VAULT' (maybe in its Archive, which op:// references can still match). Delete it permanently first, or use another title"
fi

gen() { od -An -tx1 -N32 /dev/urandom | tr -d ' \n'; }
owner="$(gen)"; sync="$(gen)"; web="$(gen)"
[[ ${#owner} -eq 64 && ${#sync} -eq 64 && ${#web} -eq 64 ]] || die "couldn't generate passwords"

# A Secure Note with concealed fields: it has no required built-in field (a
# Password item does, and keeps that field's label as "password").
# printf is a shell builtin, so the values never show up in ps.
printf '{"title":"%s","category":"SECURE_NOTE","fields":[
  {"id":"owner_password","label":"owner_password","type":"CONCEALED","value":"%s"},
  {"id":"sync_password","label":"sync_password","type":"CONCEALED","value":"%s"},
  {"id":"web_password","label":"web_password","type":"CONCEALED","value":"%s"}]}' \
  "$ITEM" "$owner" "$sync" "$web" | op item create --vault "$OP_VAULT" - >/dev/null
unset owner sync web
for f in owner_password sync_password web_password; do
  op read "op://$OP_VAULT/$ITEM/$f" >/dev/null 2>&1 \
    || die "saved '$ITEM', but op://$OP_VAULT/$ITEM/$f doesn't resolve; fix the field name in 1Password"
done
echo "Stored owner_password, sync_password, web_password in 1Password ($OP_VAULT / $ITEM)."
