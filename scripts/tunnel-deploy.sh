#!/usr/bin/env bash
# tunnel-deploy.sh: install or update the Cloudflare Tunnel on the droplet. Run on your Mac.
#
#   scripts/tunnel-deploy.sh cody@<droplet-ip>   # first install, over the temporary port 22
#   scripts/tunnel-deploy.sh finance             # later updates, through the tunnel (~/.ssh/config alias)
#
# Copies deploy/cloudflared/* and scripts/tunnel-install.sh to a private temp
# dir on the droplet, sends the tunnel credentials from 1Password into it
# (never written to disk on this Mac), and runs the installer with sudo.
# Updates are confirmed with a fresh SSH login, or rolled back on the droplet.

# $dir (a validated remote temp path) is meant to expand locally, before ssh sends it.
# shellcheck disable=SC2029

set -euo pipefail

DEST="${1:?usage: scripts/tunnel-deploy.sh <ssh-destination>}"
OP_VAULT="Finance App"
OP_ITEM=cloudflared-tunnel

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
config=deploy/cloudflared/config.yml
if grep -qE '^[^#]*REPLACE_ME' "$config"; then
  die "fill in the REPLACE_ME values in $config first (see deploy/cloudflared/README.md)"
fi
command -v op >/dev/null || die "1Password CLI (op) not found"
if command -v cloudflared >/dev/null; then
  cloudflared tunnel --config "$config" ingress validate >/dev/null || die "$config is invalid"
fi

# Held in memory only; printf is a shell builtin, so it never shows up in ps.
creds="$(op document get "$OP_ITEM" --vault "$OP_VAULT")"
[[ "$creds" == *'"TunnelSecret"'* ]] || die "1Password item '$OP_ITEM' doesn't look like tunnel credentials"

dir="$(ssh "$DEST" 'mktemp -d')"
[[ "$dir" =~ ^/tmp/tmp\.[A-Za-z0-9]+$ ]] || die "unexpected remote temp dir: $dir"
scp -q "$config" deploy/cloudflared/cloudflared.service scripts/tunnel-install.sh "$DEST:$dir/"
printf '%s\n' "$creds" | ssh "$DEST" "umask 077 && cat > $dir/tunnel.json"
unset creds

# On an update the installer restarts cloudflared, which drops this session if
# it rides the tunnel, so a failure exit here isn't conclusive by itself.
rc=0
ssh -t "$DEST" "sudo bash $dir/tunnel-install.sh $dir" || rc=$?

# Retry from fresh connections: after an update the tunnel takes a few seconds
# to come back. (Through port 22 this proves nothing about the tunnel, which is
# why updates should go through the tunnel alias.)
seen_pending=false
for attempt in 1 2 3 4 5 6 7 8; do
  pending=0
  ssh "$DEST" "systemctl is-active --quiet cloudflared-rollback.timer" || pending=$?
  case "$pending" in
    0)   # An update is waiting. Logging in again proved the new config works;
         # also require cloudflared to be up (connected) before confirming.
         $seen_pending || echo "Confirming the update from a fresh SSH session..."
         seen_pending=true
         if ssh -t "$DEST" "rm -rf $dir; systemctl is-active --quiet cloudflared &&
             sudo systemctl stop cloudflared-rollback.timer"; then
           echo "Update confirmed."
           exit 0
         fi ;;
    255) ;;  # couldn't connect (yet)
    *)   # Nothing (left) to confirm: a first install, the installer bailed out,
         # or the rollback already fired.
         ssh "$DEST" "rm -rf $dir" || true
         $seen_pending && die "the update rolled back before it was confirmed"
         exit "$rc" ;;
  esac
  echo "not confirmed yet (attempt $attempt); retrying in 10s"
  sleep 10
done
die "couldn't confirm; the droplet goes back to the previous tunnel config 5 minutes after the update"
