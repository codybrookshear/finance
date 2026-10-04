#!/usr/bin/env bash
# simplefin-claim.sh: claim a SimpleFIN setup token and store the access URL
# straight in 1Password. The URL is never printed, copied, written to disk, or
# put in a command line.
#
#   make claim        # your real setup token (prompts for it) → item "simplefin"
#   make demo-claim   # SimpleFIN's public demo token → item "simplefin-demo"
#
# A setup token can be claimed only once, so everything that could make the
# 1Password write fail is checked before claiming.

set -euo pipefail

ITEM="${1:?usage: scripts/simplefin-claim.sh <1Password item title>}"
OP_VAULT="Finance App"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
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

# Held in memory only; printf is a shell builtin, so it never shows up in ps.
url="$(go run ./cmd/sync claim)"
[[ "$url" =~ ^https://[^[:space:]\"\\]+$ ]] || die "the claim didn't return an access URL"
# A Secure Note with one concealed field: it has no required built-in field
# (a Password item does, and keeps that field's label as "password").
if ! printf '{"title":"%s","category":"SECURE_NOTE","fields":[{"id":"access_url","label":"access_url","type":"CONCEALED","value":"%s"}]}' \
    "$ITEM" "$url" | op item create --vault "$OP_VAULT" - >/dev/null; then
  die "couldn't save to 1Password. A real setup token is now used up: in SimpleFIN Bridge, revoke the new connection and make a fresh setup token, then re-run. (The demo token can be re-claimed.)"
fi
unset url
op read "op://$OP_VAULT/$ITEM/access_url" >/dev/null 2>&1 \
  || die "saved '$ITEM', but op://$OP_VAULT/$ITEM/access_url doesn't resolve; fix the field name in 1Password"
echo "Stored the access URL in 1Password ($OP_VAULT / $ITEM / access_url)."
