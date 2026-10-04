# The app on the droplet

Postgres, the SimpleFIN sync on a systemd timer (`finance-sync.timer`, every
3 hours), and the web UI at `finance.brookshear.party`.

**Design choices.**
- Images are built on your workstation from the committed HEAD and copied over
  SSH. The repo is private, so this avoids keeping a registry token on the
  droplet. The only image the droplet pulls is the digest-pinned Postgres.
- Secrets go straight from 1Password (`env/prod.env` refs) to
  `/etc/finance/secrets` (directory root 0700). Compose mounts each container's
  secrets as files under `/run/secrets`.
- Only the sync container can reach the internet. Postgres has no published port.
- The web container has no published port and no internet either. It serves a
  Unix socket in `/run/finance-web`, which cloudflared (on the host) connects
  to. Cloudflare Access checks the login at the edge, cloudflared checks it
  again, and the app checks it a third time, against signing keys that
  `finance-access-keys.timer` fetches hourly into `/etc/finance/access`.

## First deploy

On your workstation, signed in to 1Password (`eval "$(op signin)"`):

1. Database passwords, once: `scripts/db-passwords-create.sh finance-prod-db`
2. Personal settings, once: `scripts/prod-config-create.sh <manual-accounts.json> <your Access email>`
   (balances SimpleFIN can't see, like your home; start from
   `manual-accounts.example.json`, or `[]`). Later, edit item
   `finance-prod-config` in 1Password and redeploy.
3. SimpleFIN: in SimpleFIN Bridge, create a setup token, then run `make claim`
   and paste it. The access URL goes straight to 1Password item `simplefin`.
   Don't paste the token anywhere else: it's single-use, and whoever claims it
   first can read your accounts.
4. For the web UI, in the Cloudflare dashboard:
   - Access controls → Applications → Create → Self-hosted, hostname
     `finance.<your-domain>`, your Allow policy, a login method.
   - DNS: CNAME `finance` → `<tunnel-id>.cfargotunnel.com`, proxied.
   - Fill in `REPLACE_ME_WEB_AUD` (in `compose.prod.yaml` and
     `deploy/cloudflared/config.yml`; the redirect trick in
     `deploy/cloudflared/README.md` finds it), and commit.
5. `scripts/deploy.sh`. It asks for your sudo password once, runs a first
   sync, enables the timers, starts the web app and checks it refuses requests
   without a login.
6. `scripts/tunnel-deploy.sh finance` routes the hostname to the web app.

The first syncs backfill history 45 days at a time, 2 windows per run, so it
fills in over a day or two. A bank connected later gets its history the same
way, starting with the first sync that sees it. Balance history (for net worth) starts with the
first sync.

## Day to day

- Sync logs: `ssh -t finance sudo journalctl -u finance-sync`
- Web logs: `ssh -t finance 'cd /opt/finance && sudo docker compose logs --tail 50 web'`
- Next sync: `ssh -t finance sudo systemctl list-timers finance-sync.timer`
- Sync now: `ssh -t finance sudo systemctl start finance-sync`
- Update: commit, then `scripts/deploy.sh` again. It keeps the previous
  version's images, so rolling back is: set `FINANCE_VERSION` in
  `/opt/finance/.env` back to the old version, then
  `cd /opt/finance && sudo docker compose up -d web` and run the sync.

## Data

The database lives in the Docker volume `finance_pgdata`. Turn on DigitalOcean
backups (Droplet → Backups) until there's a proper `pg_dump` schedule.
