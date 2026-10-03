#!/usr/bin/env bash
# bootstrap-droplet.sh: one-time hardening for a fresh Ubuntu 24.04 droplet.
#
# Run as root, interactively (it asks you to set the admin user's sudo password):
#   scp scripts/bootstrap-droplet.sh root@<droplet-ip>:
#   ssh -t root@<droplet-ip> 'ADMIN_USER=cody bash bootstrap-droplet.sh'
#
# What it does:
#   - Full OS upgrade; automatic security updates (with 04:30 reboot when needed)
#   - Non-root admin user (SSH key copied from root, sudo WITH a password)
#   - SSH: keys only, no root login, only the admin user, no forwarding
#   - ufw: deny all inbound except SSH (DO Cloud Firewall is the outer layer)
#   - Small swapfile, a few kernel hardening sysctls
#   - Docker Engine + Compose from Docker's signed apt repo, hardened daemon config
#   - cloudflared from Cloudflare's signed apt repo (installed, NOT configured yet)
#
# It does not open any ports, and it does not store any secrets.
# Safe to re-run.

set -euo pipefail

ADMIN_USER="${ADMIN_USER:-cody}"
SWAP_SIZE_MB="${SWAP_SIZE_MB:-1024}"
APP_DIR=/opt/finance
LOG=/var/log/bootstrap-droplet.log

# Docker's published repo signing key fingerprint (docs.docker.com/engine/install/ubuntu).
DOCKER_KEY_FPR="9DC858229FC7DD38854AE2D88D81803C0EBFCD88"

exec > >(tee -a "$LOG") 2>&1
step() { printf '\n==> %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
# shellcheck disable=SC1091
. /etc/os-release
[[ "${ID:-}" == ubuntu && "${VERSION_ID:-}" == 24.04 ]] || die "expected Ubuntu 24.04, found ${PRETTY_NAME:-unknown}"
[[ "$ADMIN_USER" =~ ^[a-z][a-z0-9_-]{0,30}$ ]] || die "ADMIN_USER must be a simple lowercase name"
[[ -s /root/.ssh/authorized_keys ]] || die "/root/.ssh/authorized_keys is empty; create the droplet with an SSH key"
[[ -t 0 ]] || die "run interactively (ssh -t) so you can set the sudo password"

export DEBIAN_FRONTEND=noninteractive
APT_OPTS=(-y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)

# ---------------------------------------------------------------------------
step "Upgrading the OS"
apt-get update -q
apt-get "${APT_OPTS[@]}" full-upgrade
apt-get "${APT_OPTS[@]}" install ca-certificates curl gnupg ufw unattended-upgrades apt-listchanges
apt-get "${APT_OPTS[@]}" autoremove

# ---------------------------------------------------------------------------
step "Enabling automatic security updates"
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF
cat > /etc/apt/apt.conf.d/52unattended-upgrades-local <<'EOF'
// Security updates only (Ubuntu default origins), plus Docker and Cloudflare repos.
Unattended-Upgrade::Origins-Pattern {
        "origin=Docker,label=Docker CE";
        "origin=cloudflared";
};
Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "04:30";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
EOF
systemctl enable --now unattended-upgrades

# ---------------------------------------------------------------------------
step "Creating admin user '$ADMIN_USER'"
if ! id "$ADMIN_USER" >/dev/null 2>&1; then
  adduser --disabled-password --gecos "" "$ADMIN_USER"
fi
usermod -aG sudo "$ADMIN_USER"
install -d -m 700 -o "$ADMIN_USER" -g "$ADMIN_USER" "/home/$ADMIN_USER/.ssh"
install -m 600 -o "$ADMIN_USER" -g "$ADMIN_USER" /root/.ssh/authorized_keys "/home/$ADMIN_USER/.ssh/authorized_keys"
# Deliberately NOT in the docker group: docker group membership == root.
if passwd -S "$ADMIN_USER" | awk '{exit !($2=="L" || $2=="NP")}'; then
  echo "Set a sudo password for $ADMIN_USER (generate it in 1Password and save it there):"
  until passwd "$ADMIN_USER"; do echo "try again"; done
else
  echo "$ADMIN_USER already has a password; leaving it."
fi

# ---------------------------------------------------------------------------
step "Hardening SSH"
# sshd uses the FIRST value it sees for each option, so this file must sort
# before Ubuntu/cloud-init's 50-cloud-init.conf.
cat > /etc/ssh/sshd_config.d/00-hardening.conf <<EOF
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthenticationMethods publickey
AllowUsers $ADMIN_USER
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding no
PermitTunnel no
MaxAuthTries 3
LoginGraceTime 30
ClientAliveInterval 300
ClientAliveCountMax 2
EOF
sshd -t || die "sshd config test failed; not reloading"
systemctl reload ssh
# Root's key is no longer usable for SSH; remove it so it can't be re-enabled by accident.
: > /root/.ssh/authorized_keys

# ---------------------------------------------------------------------------
step "Configuring host firewall (ufw)"
ufw --force reset >/dev/null
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp comment 'temporary: bootstrap SSH (DO firewall limits source IP)'
ufw --force enable
# Note: we publish no Docker ports, so Docker's iptables rules can't bypass ufw.
# The DO Cloud Firewall (zero inbound rules) remains the real perimeter.

# ---------------------------------------------------------------------------
step "Swap (${SWAP_SIZE_MB} MB) and kernel settings"
if ! swapon --show | grep -q /swapfile; then
  fallocate -l "${SWAP_SIZE_MB}M" /swapfile
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi
cat > /etc/sysctl.d/90-hardening.conf <<'EOF'
vm.swappiness = 10
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
kernel.unprivileged_bpf_disabled = 1
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv6.conf.default.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv4.tcp_syncookies = 1
EOF
sysctl --system >/dev/null

# ---------------------------------------------------------------------------
step "Installing Docker Engine (Docker's signed repo)"
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /tmp/docker.asc
got_fpr="$(gpg --show-keys --with-colons /tmp/docker.asc | awk -F: '/^fpr/{print $10; exit}')"
[[ "$got_fpr" == "$DOCKER_KEY_FPR" ]] || die "Docker signing key fingerprint mismatch ($got_fpr)"
install -m 0644 /tmp/docker.asc /etc/apt/keyrings/docker.asc
rm -f /tmp/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update -q
apt-get "${APT_OPTS[@]}" install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

install -d -m 0755 /etc/docker
cat > /etc/docker/daemon.json <<'EOF'
{
  "log-driver": "local",
  "log-opts": { "max-size": "10m", "max-file": "3" },
  "live-restore": true,
  "no-new-privileges": true,
  "userland-proxy": false
}
EOF
systemctl enable docker >/dev/null
systemctl restart docker
docker info --format '{{.ServerVersion}}' >/dev/null || die "docker failed to start"

# ---------------------------------------------------------------------------
step "Installing cloudflared (Cloudflare's signed repo; configured later)"
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
chmod 0644 /usr/share/keyrings/cloudflare-main.gpg
echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" \
  > /etc/apt/sources.list.d/cloudflared.list
apt-get update -q
apt-get "${APT_OPTS[@]}" install cloudflared

# ---------------------------------------------------------------------------
step "Creating $APP_DIR"
install -d -m 0750 -o root -g "$ADMIN_USER" "$APP_DIR"

# ---------------------------------------------------------------------------
step "Done"
cat <<EOF

Bootstrap complete. Log: $LOG

  Docker:      $(docker --version)
  Compose:     $(docker compose version --short)
  cloudflared: $(cloudflared --version 2>&1 | head -1)

BEFORE closing this session, in a NEW terminal check you can still get in:
    ssh $ADMIN_USER@<droplet-ip>
    sudo -v            # should ask for the password you just set

Root SSH login is now disabled. If you're locked out, use the DigitalOcean
Recovery Console.

Next: set up the Cloudflare Tunnel and Access SSH, then close port 22
(deploy/cloudflared/README.md in the repo).
EOF
if [[ -f /var/run/reboot-required ]]; then
  echo
  echo "A reboot is required (kernel update). After verifying the new login: sudo reboot"
fi
