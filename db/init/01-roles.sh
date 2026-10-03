#!/bin/sh
# Runs once, when the Postgres data volume is first initialized.
# Creates the application roles with passwords read from Docker secrets.
# psql reads the files itself (\set with backticks), so passwords never
# appear in an environment variable or on a command line.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\set sync_pw `cat /run/secrets/finance_sync_db_password`
\set web_pw  `cat /run/secrets/finance_web_db_password`
SELECT format('CREATE ROLE finance_sync LOGIN PASSWORD %L', :'sync_pw') \gexec
SELECT format('CREATE ROLE finance_web  LOGIN PASSWORD %L', :'web_pw')  \gexec
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database()) \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO finance_sync, finance_web', current_database()) \gexec
SQL
