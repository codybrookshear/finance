-- Least-privilege grants. Roles are created (with passwords) by
-- db/init/01-roles.sh before migrations run.
--   finance_sync: the only role that writes bank data.
--   finance_web : read-only (UI, API, MCP).

REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO finance_sync, finance_web;

REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;

GRANT SELECT ON ALL TABLES IN SCHEMA public TO finance_web;

GRANT SELECT, INSERT, UPDATE, DELETE
    ON connections, balance_snapshots, sync_runs, sync_state
    TO finance_sync;

-- accounts/transactions: sync may only change the bank-sourced columns,
-- never the user-maintained ones (names, categories, notes, flags).
GRANT SELECT, INSERT ON accounts, transactions TO finance_sync;
GRANT DELETE ON transactions TO finance_sync;   -- stale pending rows only, in practice
GRANT UPDATE (connection_id, org_name, name, currency, balance, available_balance, balance_at, updated_at)
    ON accounts TO finance_sync;
GRANT UPDATE (posted_at, transacted_at, amount, description, payee, memo, pending, updated_at)
    ON transactions TO finance_sync;
GRANT SELECT ON categories, rules TO finance_sync;
GRANT USAGE ON SEQUENCE sync_runs_id_seq TO finance_sync;

-- The migration bookkeeping table is owner-only.
REVOKE ALL ON schema_migrations FROM finance_sync, finance_web;
