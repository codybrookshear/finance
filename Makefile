.PHONY: setup test lint vuln dev demo-claim claim sync-once

DEMO_TOKEN := aHR0cHM6Ly9iZXRhLWJyaWRnZS5zaW1wbGVmaW4ub3JnL3NpbXBsZWZpbi9jbGFpbS9ERU1PLXYyLTVERTZDQjUzQjU2Q0Q4NUJFMUQ2

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

## Local stack with secrets from 1Password.
dev:
	op run --env-file=env/dev.env -- docker compose up --build

## Claim SimpleFIN's public demo token. The access URL goes to your
## clipboard (cleared after 60s): paste it into 1Password item
## "simplefin-demo", field "access_url".
demo-claim:
	@echo "$(DEMO_TOKEN)" | go run ./cmd/sync claim | pbcopy
	@echo "Demo access URL copied to clipboard; clearing in 60s."
	@(sleep 60; pbcopy </dev/null) &

## Claim your REAL setup token (prompts for it; never put it on the command line).
## Paste the result into 1Password item "simplefin", field "access_url".
claim:
	@go run ./cmd/sync claim | pbcopy
	@echo "Access URL copied to clipboard; clearing in 60s. Store it in 1Password now."
	@(sleep 60; pbcopy </dev/null) &
