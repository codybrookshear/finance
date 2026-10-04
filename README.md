# finance

Personal finance app: SimpleFIN Bridge → Postgres → (soon) a phone web app
behind Cloudflare Access, and a local MCP server for Claude.

**Status:** step 1. Schema + SimpleFIN sync job. Web UI, Cloudflare and MCP come next.

## Security model

- **Read-only data source.** SimpleFIN can't move money; bank logins live at
  SimpleFIN Bridge, never here.
- **The access URL is the crown jewel.** It's held only by the `sync` container,
  arrives as a Docker secret file, is split into host + Basic-auth creds in
  memory, and is never in URLs, logs or errors (tested).
- **Least-privilege database roles:**

  | role | can |
  |---|---|
  | `finance_owner` | schema migrations only (`migrate` container) |
  | `finance_sync`  | write bank-sourced columns; **cannot** touch categories, notes, user flags, rules (it may run `categorize()`) |
  | `finance_web`   | read-only (UI, API, MCP); may `SET ROLE finance_edit` for edits, but doesn't inherit it |
  | `finance_edit`  | no login. The UI's edit requests: category, transfer flag, note, and rules only |
  | `finance_rules` | no login. Owns `categorize()` (rules + transfer detection); can change category columns only |

- **Network:** Postgres sits on an `internal` Docker network with no internet route
  and no published ports. Only `sync` can reach the internet.
- **No open ports.** SSH (and later the web app) comes in through a Cloudflare Tunnel
  behind Cloudflare Access; cloudflared re-checks the Access JWT before connecting
  to sshd. See [deploy/cloudflared/README.md](deploy/cloudflared/README.md).
- **Containers:** distroless, non-root, read-only filesystem, all capabilities dropped.
- **Secrets:** 1Password is the source of truth. `env/*.env` contain only
  `op://` references and are safe to commit. A gitleaks pre-commit hook blocks
  accidental secret commits.

## Layout

```
cmd/sync        sync job (run | claim)
cmd/migrate     applies migrations as the owner role
internal/simplefin  credential-safe SimpleFIN client
internal/syncer     incremental + backfill logic
internal/store      Postgres access, migration runner
migrations/     SQL schema + grants (embedded in the migrate binary)
db/init/        creates app roles on first DB start
deploy/cloudflared  tunnel config + hardened systemd unit (runbook inside)
scripts/        droplet bootstrap, tunnel create/deploy/install
```

## Getting started

Prereqs: Go 1.27+ (an older Go fetches it automatically), Docker, 1Password CLI (`op`), `gitleaks`.

```sh
make setup        # git hook; go mod tidy creates go.sum, verified against sum.golang.org
make test         # unit + integration tests against a throwaway Postgres
```

### 1Password items (vault "Finance App")

| item | fields |
|---|---|
| `finance-dev-db` | `owner_password`, `sync_password`, `web_password` (`scripts/db-passwords-create.sh finance-dev-db`) |
| `simplefin-demo` | `access_url` (from `make demo-claim`) |
| `finance-prod-db`, `simplefin` | same fields, for production later |

### Run locally with SimpleFIN demo data

```sh
eval "$(op signin)"   # 1Password CLI session for this shell
make demo-claim       # access URL → 1Password simplefin-demo/access_url
make dev              # postgres + migrate + one sync pass
```

## How sync works

Each run: one incremental fetch (last success − 5 days → now, including pending)
that also records each account's balance for today, then up to 3 backfill requests
walking back in 89-day windows until two windows come back empty or 3 years is reached.
At ~4 runs/day that stays under SimpleFIN Bridge's ~24 requests/day guidance.

- Pending transactions that disappear (posted under a new ID, or voided) are removed.
- Balance history only exists from the first sync onward, so deploy sync early.
- User fields (categories, notes, transfer flags, account display names) are never
  overwritten by sync, which is enforced by column-level grants, not just code.

## Connecting your real accounts (later, on the server)

Generate a setup token in SimpleFIN Bridge only when you're ready, then `make claim`
(it prompts; never pass the token as an argument) and store the result in 1Password
item `simplefin`. Setup tokens are single-use. If the access URL ever leaks, revoke the
app in SimpleFIN Bridge and claim a new token.
