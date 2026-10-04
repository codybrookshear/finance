.PHONY: setup test lint vuln dev demo-claim claim sync-once

# SimpleFIN's public, reusable demo setup token (fake data). It decodes to
# https://beta-bridge.simplefin.org/simplefin/claim/DEMO, which returns
# https://demo:demo@beta-bridge.simplefin.org/simplefin.
DEMO_TOKEN := aHR0cHM6Ly9iZXRhLWJyaWRnZS5zaW1wbGVmaW4ub3JnL3NpbXBsZWZpbi9jbGFpbS9ERU1P # gitleaks:allow

## One-time: secret-scanning git hook + resolve dependencies.
setup:
	git config core.hooksPath .githooks
	go mod tidy
	go mod verify

## Unit + integration tests (throwaway Postgres in Docker, loopback only).
test:
	sh scripts/test.sh

lint:
	gofmt -l . | (! grep .) || (echo "gofmt needed"; exit 1)
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## Local stack with SimpleFIN demo data; the UI is on http://<this machine>:8080.
## Secrets go from 1Password into files in a private RAM directory (cleared on
## logout/reboot), which compose mounts into the containers.
## Sign in first: eval "$$(op signin)"
DEV_SECRETS := $(or $(XDG_RUNTIME_DIR),$(TMPDIR))/finance-dev-secrets
dev:
	@scripts/secrets-write.sh env/dev.env "$(DEV_SECRETS)"
	FINANCE_SECRETS_DIR="$(DEV_SECRETS)" docker compose -p finance-dev -f compose.yaml -f compose.dev.yaml up --build

## Claim SimpleFIN's public demo token; the (fake-data) access URL goes straight
## into 1Password item "simplefin-demo", field "access_url". Sign in first:
## eval "$$(op signin)"
demo-claim:
	@echo "$(DEMO_TOKEN)" | scripts/simplefin-claim.sh simplefin-demo

## Claim your REAL setup token (prompts for it; never put it on the command line)
## and store the access URL in 1Password item "simplefin", field "access_url".
claim:
	@scripts/simplefin-claim.sh simplefin
