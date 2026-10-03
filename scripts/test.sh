#!/bin/sh
# Runs unit + integration tests against a throwaway Postgres container that
# is bound to 127.0.0.1 only and deleted afterwards. No real secrets involved.
set -eu

NAME=finance-testdb
PORT=55432
remove_db() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
remove_db   # leftover from an interrupted run

SECRETS="$(mktemp -d)"
trap 'remove_db; rm -rf "$SECRETS"' EXIT

# Throwaway passwords; the test DB uses trust auth on loopback anyway.
echo test-sync > "$SECRETS/finance_sync_db_password"
echo test-web  > "$SECRETS/finance_web_db_password"
chmod 644 "$SECRETS"/*

docker run -d --name "$NAME" -p 127.0.0.1:$PORT:5432 \
  -e POSTGRES_USER=finance_owner -e POSTGRES_DB=finance \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -v "$PWD/db/init:/docker-entrypoint-initdb.d:ro" \
  -v "$SECRETS:/run/secrets:ro" \
  postgres:16-alpine >/dev/null

printf 'waiting for postgres'
for _ in $(seq 1 60); do
  # Wait for the *final* server (initdb runs a temporary one first).
  if docker exec "$NAME" psql -U finance_owner -d finance -tAc "select 1 from pg_roles where rolname='finance_web'" 2>/dev/null | grep -q 1 \
     && docker logs "$NAME" 2>&1 | grep -q "PostgreSQL init process complete"; then
    break
  fi
  printf .; sleep 1
done
echo

OWNER="postgres://finance_owner@127.0.0.1:$PORT/finance?sslmode=disable"
SYNC="postgres://finance_sync@127.0.0.1:$PORT/finance?sslmode=disable"

DATABASE_URL="$OWNER" go run ./cmd/migrate
TEST_DATABASE_URL="$SYNC" TEST_OWNER_DATABASE_URL="$OWNER" go test -count=1 ./...
