# Cloudflare Tunnel + Access (SSH)

When this is done, SSH to the droplet goes:

```
Mac: ssh finance
  → cloudflared access ssh       (browser login to Cloudflare Access, token cached)
  → Cloudflare edge              (Access policy: only you)
  → tunnel                       (outbound-only connection from the droplet)
  → cloudflared on the droplet   (re-checks the Access JWT itself)
  → sshd on 127.0.0.1:22         (SSH key required, as before)
```

The droplet has no open inbound ports: the DO Cloud Firewall has zero inbound
rules and ufw denies all inbound.

**Design choices.**
- cloudflared runs on the host as a sandboxed systemd service, not in Compose,
  so SSH keeps working when Docker is broken.
- The tunnel is locally managed: `config.yml` in this repo, not the dashboard,
  decides what it can reach.
- Credentials go from 1Password to `/etc/cloudflared/tunnel.json` (root, 0600),
  and systemd hands the service a private copy.

## Prerequisites

- A **personal** Cloudflare account (not a work one) with your domain on it, and
  Zero Trust enabled (the free plan is fine). Your team name is the `<team>` in
  `<team>.cloudflareaccess.com`.
- On your Mac: `brew install cloudflared`, and the 1Password CLI, signed in.
- The droplet bootstrapped with `scripts/bootstrap-droplet.sh`, with port 22 still
  allowed from your IP.

## 1. Access application (Cloudflare dashboard)

1. Zero Trust → Settings → Authentication: add a login method. One-time PIN
   works. A GitHub or Google login that you protect with a passkey is better.
2. Access → Applications → Add → Self-hosted:
   - hostname `ssh.<your-domain>`, session duration 24h
   - policy: Allow, Include → Emails → your address
3. From the app's overview, copy the **Application Audience (AUD) tag**. Put it,
   and your team name, into `config.yml`.

## 2. Create the tunnel (Mac, one time)

```sh
scripts/tunnel-create.sh ssh.<your-domain>
```

This logs in to Cloudflare in the browser (pick the personal account and zone),
then creates tunnel `finance`. It routes the hostname to the tunnel, saves the
credentials as the 1Password document `cloudflared-tunnel`, and fills in the
tunnel ID and hostname in `config.yml`. Finally it deletes
`~/.cloudflared/cert.pem`, the zone-wide login cert. Commit `config.yml`
afterwards.

## 3. Install on the droplet

```sh
scripts/tunnel-deploy.sh cody@<droplet-ip>
```

It asks for your sudo password once, then waits until the tunnel is connected.

## 4. SSH through the tunnel

Add to `~/.ssh/config` on your Mac:

```
Host finance
  HostName ssh.<your-domain>
  User cody
  ProxyCommand /opt/homebrew/bin/cloudflared access ssh --hostname %h
```

The first `ssh finance` opens a browser for the Access login. You also get a
host-key prompt, because this is a new name for the same machine. Check that the
fingerprint matches `ssh-keygen -lF <droplet-ip>` before accepting.

## 5. Close port 22

Only after `ssh finance` works:

1. DigitalOcean → Networking → Firewalls → delete the SSH inbound rule, so no
   inbound rules are left. Leave outbound open: cloudflared needs TCP+UDP 7844
   and TCP 443 to Cloudflare.
2. `ssh -t finance sudo ufw delete allow 22/tcp`
3. Check: `nc -vz -G 5 <droplet-ip> 22` times out, and `ssh finance` still works.

If you're ever locked out, use the DigitalOcean Recovery Console and log in with
the admin user's password.

## Changing the config later

For example, to add the web app's hostname: edit `config.yml`, then

```sh
scripts/tunnel-deploy.sh finance
```

The droplet keeps the previous config and **rolls back automatically after 5
minutes** unless the update is confirmed. The deploy script confirms it by
logging in again through the tunnel once cloudflared has reconnected. Deploy
updates through the tunnel (`finance`), not the IP, so the confirmation proves
the tunnel works.

To route a new hostname, either add a proxied CNAME `<name>` →
`<tunnel-id>.cfargotunnel.com` in the dashboard, or run `cloudflared tunnel login`,
then `cloudflared tunnel route dns finance <hostname>`, then delete
`~/.cloudflared/cert.pem`.

## Troubleshooting

`ssh -t finance sudo journalctl -u cloudflared -n 50`, or use the Recovery Console.

- `Failed to get tunnel`: the credentials don't match a tunnel in the account.
- `no access token in request` / `Invalid token`: the Access app is missing, or
  the team name or AUD tag in `config.yml` is wrong.
- `ping_group_range` and UDP buffer-size warnings are harmless.
