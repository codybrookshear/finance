#!/usr/bin/env bash
# tunnel-install.sh: install or update the Cloudflare Tunnel service on the droplet.
#
# Run from your workstation via scripts/tunnel-deploy.sh, which copies these into a
# private temp dir on the droplet and runs this script with sudo:
#   config.yml, cloudflared.service, tunnel.json (credentials; deleted here)
#
# First install: starts the service and waits until it's connected.
# Update: keeps the previous files, restarts without waiting (this SSH session
# may be riding the tunnel), and arms an automatic rollback in 5 minutes. A
# fresh SSH login through the tunnel confirms the update (tunnel-deploy.sh does
# this); otherwise the old config comes back on its own.

set -euo pipefail

SRC="${1:?usage: tunnel-install.sh <dir with config.yml, cloudflared.service, tunnel.json>}"
ETC=/etc/cloudflared
PREV=$ETC/prev
UNIT=/etc/systemd/system/cloudflared.service
ROLLBACK=cloudflared-rollback
ROLLBACK_AFTER=5min

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
command -v cloudflared >/dev/null || die "cloudflared isn't installed; run bootstrap-droplet.sh first"
for f in config.yml cloudflared.service tunnel.json; do
  [[ -s "$SRC/$f" ]] || die "missing $SRC/$f"
done

# Get the credentials out of the temp dir first, whatever happens next.
umask 077
creds="$(mktemp)"
trap 'rm -f "$creds"' EXIT
cat "$SRC/tunnel.json" > "$creds"
rm -f "$SRC/tunnel.json"

if grep -qE '^[^#]*REPLACE_ME' "$SRC/config.yml"; then
  die "config.yml still has REPLACE_ME placeholders"
fi
# The config must name the tunnel these credentials belong to.
cred_id="$(sed -n 's/.*"TunnelID" *: *"\([0-9a-f-]*\)".*/\1/p' "$creds")"
cfg_id="$(sed -n 's/^tunnel: *\([0-9a-f-]*\).*/\1/p' "$SRC/config.yml")"
[[ -n "$cred_id" && "$cred_id" == "$cfg_id" ]] \
  || die "tunnel ID mismatch: config.yml has '${cfg_id}', credentials are for '${cred_id}'"
cloudflared tunnel --config "$SRC/config.yml" ingress validate >/dev/null \
  || die "config.yml failed 'cloudflared tunnel ingress validate'"

if systemctl is-active --quiet "$ROLLBACK.timer"; then
  die "a previous update is still unconfirmed; confirm it (sudo systemctl stop $ROLLBACK.timer) or let it roll back first"
fi

update=false
if systemctl is-active --quiet cloudflared; then
  update=true
  install -d -m 0700 "$PREV"
  cp -p "$ETC/config.yml" "$ETC/tunnel.json" "$UNIT" "$PREV/"
fi

install -d -m 0755 "$ETC"
install -m 0600 "$creds" "$ETC/tunnel.json"
install -m 0644 "$SRC/config.yml" "$ETC/config.yml"
install -m 0644 "$SRC/cloudflared.service" "$UNIT"
systemctl daemon-reload
systemctl enable --quiet cloudflared

if ! $update; then
  echo "Starting cloudflared (waits for a connection to Cloudflare, up to 60s)..."
  if ! systemctl restart cloudflared; then
    journalctl -u cloudflared -n 40 --no-pager
    die "cloudflared didn't connect; see the log above"
  fi
  echo "Connected: $(curl -fsS http://127.0.0.1:20241/ready)"
  exit 0
fi

systemd-run --quiet --unit="$ROLLBACK" --on-active="$ROLLBACK_AFTER" /bin/sh -c "
  cp -p $PREV/config.yml $PREV/tunnel.json $ETC/ &&
  cp -p $PREV/cloudflared.service $UNIT &&
  systemctl daemon-reload &&
  systemctl restart cloudflared"
systemctl restart --no-block cloudflared
cat <<EOF
Restarting cloudflared with the new config. If this session rides the tunnel, it
will drop now. The previous config comes back automatically in $ROLLBACK_AFTER
unless a NEW SSH login through the tunnel confirms with:
    sudo systemctl stop $ROLLBACK.timer
EOF
