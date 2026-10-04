#!/usr/bin/env bash
# prod-config-create.sh: store the production app's personal settings in
# 1Password (item finance-prod-config), so they never sit in the repo:
#   manual_accounts        JSON list of balances kept by hand (internal/manual)
#   access_allowed_emails  who may use the web UI, comma-separated
#
#   scripts/prod-config-create.sh <manual-accounts.json> <email>[,email...]
#
# Afterwards, change them in the 1Password app (the item's fields) and run
# scripts/deploy.sh. Values go to `op` on stdin, never on a command line.

set -euo pipefail

FILE="${1:?usage: scripts/prod-config-create.sh <manual-accounts.json> <emails>}"
EMAILS="${2:?usage: scripts/prod-config-create.sh <manual-accounts.json> <emails>}"
OP_VAULT="Finance App"
ITEM=finance-prod-config

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
command -v op >/dev/null || die "1Password CLI (op) not found"
command -v python3 >/dev/null || die "python3 not found"
op whoami >/dev/null 2>&1 || die "sign in to the 1Password CLI first: eval \"\$(op signin)\""
[[ -f "$FILE" ]] || die "no such file: $FILE"
# Same validation the sync applies, before anything is stored.
go run ./scripts/internal/checkmanual "$FILE" || die "$FILE isn't valid (see above)"
items="$(op item list --vault "$OP_VAULT" --include-archive --format json)" || die "couldn't list items in '$OP_VAULT'"
if grep -Eq "\"title\": *\"$ITEM\"" <<<"$items"; then
  die "1Password already has '$ITEM' (maybe archived); edit it there instead"
fi

python3 - "$ITEM" "$FILE" "$EMAILS" <<'PY' | op item create --vault "$OP_VAULT" - >/dev/null
import json, sys
title, path, emails = sys.argv[1:]
print(json.dumps({"title": title, "category": "SECURE_NOTE", "fields": [
    {"id": "manual_accounts", "label": "manual_accounts", "type": "STRING", "value": open(path).read().strip()},
    {"id": "access_allowed_emails", "label": "access_allowed_emails", "type": "STRING", "value": emails.strip()},
]}))
PY
for f in manual_accounts access_allowed_emails; do
  op read "op://$OP_VAULT/$ITEM/$f" >/dev/null 2>&1 || die "saved '$ITEM', but op://$OP_VAULT/$ITEM/$f doesn't resolve"
done
echo "Stored manual_accounts and access_allowed_emails in 1Password ($OP_VAULT / $ITEM)."
