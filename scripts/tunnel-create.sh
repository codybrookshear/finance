#!/usr/bin/env bash
# tunnel-create.sh: one-time, on your Mac. Creates the tunnel, points the SSH
# hostname at it, saves its credentials to 1Password, and fills in the tunnel
# ID and hostname in deploy/cloudflared/config.yml.
#
#   scripts/tunnel-create.sh ssh.example.com
#
# `cloudflared tunnel login` writes ~/.cloudflared/cert.pem, which can create
# and delete tunnels and DNS records for the whole zone. This script deletes it
# on exit. The tunnel credentials go to a temp dir, then 1Password, and are
# never left in ~/.cloudflared.

set -euo pipefail

HOST="${1:?usage: scripts/tunnel-create.sh <ssh hostname, e.g. ssh.example.com>}"
NAME="${TUNNEL_NAME:-finance}"
OP_VAULT="Finance App"
OP_ITEM=cloudflared-tunnel
CERT="$HOME/.cloudflared/cert.pem"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
config=deploy/cloudflared/config.yml
[[ "$HOST" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] || die "not a hostname: $HOST"
command -v cloudflared >/dev/null || die "cloudflared not found (brew install cloudflared)"
command -v op >/dev/null || die "1Password CLI (op) not found"
op whoami >/dev/null 2>&1 || die "sign in to the 1Password CLI first"
if op document get "$OP_ITEM" --vault "$OP_VAULT" >/dev/null 2>&1; then
  die "1Password already has '$OP_ITEM' in '$OP_VAULT'; the tunnel was already created"
fi
[[ ! -e "$CERT" ]] || die "$CERT already exists (from another Cloudflare account?); move it aside first"

work="$(mktemp -d)"
trap 'rm -rf "$work"; rm -f "$CERT"' EXIT

echo "A browser window will open. Log in to your PERSONAL Cloudflare account and"
echo "authorize the zone that $HOST belongs to."
cloudflared tunnel login

cloudflared tunnel create --credentials-file "$work/tunnel.json" "$NAME"
if ! op document create "$work/tunnel.json" --vault "$OP_VAULT" --title "$OP_ITEM" \
    --file-name tunnel.json >/dev/null; then
  die "couldn't save the credentials to 1Password. They're gone now: run 'cloudflared tunnel login', 'cloudflared tunnel delete $NAME', delete ~/.cloudflared/cert.pem, then re-run this script"
fi
echo "Saved the tunnel credentials to 1Password ($OP_VAULT / $OP_ITEM)."

# If $HOST isn't in the zone you authorized, cloudflared appends that zone's
# name instead of failing, so check that it routed exactly $HOST.
out="$(cloudflared tunnel route dns "$NAME" "$HOST" 2>&1)" || { echo "$out"; die "DNS route failed"; }
echo "$out"
if ! grep -qF -e "CNAME $HOST which" -e "$HOST is already configured" <<<"$out"; then
  die "cloudflared routed a different hostname than $HOST (wrong zone?). Delete that DNS record in the dashboard"
fi

id="$(sed -n 's/.*"TunnelID" *: *"\([0-9a-f-]*\)".*/\1/p' "$work/tunnel.json")"
[[ -n "$id" ]] || die "couldn't read the tunnel ID from the credentials"
sed -i.bak -e "s/REPLACE_ME_TUNNEL_UUID/$id/" -e "s/REPLACE_ME_SSH_HOSTNAME/$HOST/" "$config"
rm -f "$config.bak"

cat <<EOF

Tunnel '$NAME' ($id) created; $HOST routes to it.
Filled in the tunnel ID and hostname in $config. The team name and AUD tag
come from the Access app (deploy/cloudflared/README.md).
EOF
