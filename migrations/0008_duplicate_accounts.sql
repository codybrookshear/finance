-- A joint account that two bank logins both report arrives twice from
-- SimpleFIN, with a different account ID per connection. The sync marks the
-- later copy as a duplicate of the first (store.MarkDuplicateAccounts); the
-- app shows, counts and lists only the first. Same account = same name and
-- currency at the same bank (the connection's org_url host), or, when a
-- connection has no org_url, same name and the same non-zero balance.
ALTER TABLE accounts ADD COLUMN duplicate_of text REFERENCES accounts(id) ON DELETE SET NULL;
GRANT UPDATE (duplicate_of) ON accounts TO finance_sync;

-- Account settings aren't editable in the UI after all: take back 0007's grant.
REVOKE UPDATE (display_name, include_in_net_worth, hidden) ON accounts FROM finance_edit;
