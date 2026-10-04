# Sync on the droplet

Postgres plus the SimpleFIN sync, run by a systemd timer four times a day
(`finance-sync.timer`). The web UI isn't deployed yet.

**Design choices.**
- Images are built on your workstation from the committed HEAD and copied over
  SSH. The repo is private, so this avoids keeping a registry token on the
  droplet. The only image the droplet pulls is the digest-pinned Postgres.
- Secrets go straight from 1Password (`env/prod.env` refs) to
  `/etc/finance/secrets` (directory root 0700). Compose mounts each container's
  secrets as files under `/run/secrets`.
- Only the sync container can reach the internet. Postgres has no published port.

## First deploy

On your workstation, signed in to 1Password (`eval "$(op signin)"`):

1. Database passwords, once: `scripts/db-passwords-create.sh finance-prod-db`
2. SimpleFIN: in SimpleFIN Bridge, create a setup token, then run `make claim`
   and paste it. The access URL goes straight to 1Password item `simplefin`.
   Don't paste the token anywhere else: it's single-use, and whoever claims it
   first can read your accounts.
3. `scripts/sync-deploy.sh`. It asks for your sudo password once, runs a first
   sync, and enables the timer.

The first runs backfill history 45 days at a time, 3 windows per run, so it
fills in over a day or two. Balance history (for net worth) starts with the
first sync.

## Day to day

- Logs: `ssh -t finance sudo journalctl -u finance-sync`
- Next run: `ssh -t finance sudo systemctl list-timers finance-sync.timer`
- Sync now: `ssh -t finance sudo systemctl start finance-sync`
- Update: commit, then `scripts/sync-deploy.sh` again. It keeps the previous
  version's images, so rolling back is: set `FINANCE_VERSION` in
  `/opt/finance/.env` back to the old version, then run the sync.

## Data

The database lives in the Docker volume `finance_pgdata`. Turn on DigitalOcean
backups (Droplet → Backups) until there's a proper `pg_dump` schedule.
