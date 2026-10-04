# finance: personal finance app

Personal app: SimpleFIN Bridge → Postgres → web UI (phone) + local MCP server for Claude.
Single user (me). Security is the top priority; prefer fewer dependencies.

## Architecture (decided)
- Go. DigitalOcean droplet (Ubuntu 24.04, SFO3) running Docker Compose.
- Access: Cloudflare Tunnel + Cloudflare Access (no open inbound ports; DO Cloud Firewall
  has zero inbound rules once the tunnel works). App must verify `Cf-Access-Jwt-Assertion`.
- cloudflared runs on the HOST (sandboxed systemd unit), not in Compose: it's the SSH path
  and must survive Docker breaking. Locally-managed tunnel: ingress lives in
  `deploy/cloudflared/config.yml` (git), and every rule sets `originRequest.access` so
  cloudflared re-verifies the Access JWT. Runbook: `deploy/cloudflared/README.md`.
- Web: server-rendered `net/http` + `html/template` + htmx + uPlot. No third-party
  scripts/CDNs, strict CSP, `Cache-Control: no-store`. Passkey login as inner layer.
- MCP: local Go server on my Mac (official MCP Go SDK) calling /api with a Cloudflare
  Access service token kept in macOS Keychain. Read-only tools.
- Secrets: 1Password is the source of truth; env/*.env hold only op:// refs.
  `scripts/secrets-write.sh` writes each one to a file in a private dir (RAM locally);
  compose mounts them as file-backed secrets under /run/secrets, never env vars
  (env-sourced Compose secrets don't work with read_only services).
- Images: GHCR, built by GitHub Actions.

## Security invariants (don't break these)
- SimpleFIN access URL is held only by the `sync` container; never logged or put in errors.
- DB roles: finance_owner (migrations only), finance_sync (bank columns only, via
  column-level grants), finance_web (read-only; edits only via `SET LOCAL ROLE finance_edit`,
  which it doesn't inherit), finance_edit (category/transfer/note columns + rules),
  finance_rules (owns `categorize()`, category columns only). New tables need explicit grants
  in a migration.
- Categories set by hand ('manual'/'claude') are never changed by rules or detection.
- Sync must never overwrite user fields (categories, notes, is_transfer, display names).
- Postgres stays on the internal Docker network; only `sync` has egress. `web` is on the
  internal network only, so it can't publish a port: in production it listens on a Unix
  socket that cloudflared (host) connects to. compose.dev.yaml (dev only) adds a LAN port.
- Every tunnel ingress rule (except the 404 catch-all) has `access: required: true`.
- Money is NUMERIC / string decimals, never float64.

## Status
- Done: schema + grants, SimpleFIN client, sync job (incremental + backfill + pending
  reconciliation + daily balance snapshots), compose, CI, droplet bootstrap script,
  tunnel config + scripts.
- Droplet `app-01` bootstrapped; Cloudflare Tunnel + Access SSH live (`ssh finance`);
  port 22 closed at the DO firewall (2026-10-03).
- Admin workstation is an Ubuntu desktop (not a Mac); droplet SSH key `~/.ssh/finance_ed25519`.
  Domain: `brookshear.party` (Cloudflare Registrar, paid to 2028-10-03); SSH hostname `ssh.brookshear.party`.
- Web UI (search, monthly net income/spending, net worth, categorization + rules,
  transfer detection) runs locally via `make dev`
  (http://<dev box>:8080, demo data, Access check off). Not deployed yet.
- Next: deploy sync early (balance history starts accruing) → deploy web (socket,
  Access app, signing-key refresh timer) → MCP server.

## Commands
- `make setup` / `make test` (throwaway Postgres in Docker) / `make dev` / `make demo-claim`

