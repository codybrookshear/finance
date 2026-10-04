-- Account settings from the web UI: rename, count in net worth, hide (for
-- example one copy of a joint account that two bank logins both report).
-- These are user fields: finance_sync has no UPDATE on them, so a sync never
-- changes them back.
GRANT UPDATE (display_name, include_in_net_worth, hidden) ON accounts TO finance_edit;
