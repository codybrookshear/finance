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
- Secrets: 1Password is the source of truth (`op run --env-file=env/*.env`; env files hold
  only op:// refs). Containers get secrets as files under /run/secrets, never env vars.
- Images: GHCR, built by GitHub Actions.

## Security invariants (don't break these)
- SimpleFIN access URL is held only by the `sync` container; never logged or put in errors.
- DB roles: finance_owner (migrations only), finance_sync (bank columns only, via
  column-level grants), finance_web (read-only). New tables need explicit grants in a migration.
- Sync must never overwrite user fields (categories, notes, is_transfer, display names).
- Postgres stays on the internal Docker network; only `sync` has egress. (cloudflared is on
  the host; the web container will need a 127.0.0.1-only published port for it.)
- Every tunnel ingress rule (except the 404 catch-all) has `access: required: true`.
- Money is NUMERIC / string decimals, never float64.

## Status
- Done: schema + grants, SimpleFIN client, sync job (incremental + backfill + pending
  reconciliation + daily balance snapshots), compose, CI, droplet bootstrap script,
  tunnel config + scripts (tested in an Ubuntu 24.04 systemd container, not yet live).
- Droplet created; bootstrap not yet run. Port-22 rule should be my IP only (temporary).
- Next: bootstrap → Access app + `scripts/tunnel-create.sh` → `scripts/tunnel-deploy.sh` →
  remove port 22 → deploy sync early (balance history starts accruing) → web UI (search,
  monthly spending, net worth) → MCP server.

## Commands
- `make setup` / `make test` (throwaway Postgres in Docker) / `make dev` / `make demo-claim`

