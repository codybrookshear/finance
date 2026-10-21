-- A Professional category (work expenses, dues, licenses).
INSERT INTO categories (name, kind, emoji, hue) VALUES ('Professional', 'expense', '💼', 85)
ON CONFLICT (name) DO NOTHING;

-- Retirement accounts: the Accounts page can leave them out of the net worth
-- total and chart ("now" money) or count them ("retirement + now"). A user
-- field, like include_in_net_worth: sync never writes it. The first guess is
-- by name; fix any it got wrong with
--   UPDATE accounts SET retirement = <true|false> WHERE id = '...'
-- (as finance_owner, or finance_edit).
ALTER TABLE accounts ADD COLUMN retirement boolean NOT NULL DEFAULT false;
GRANT UPDATE (retirement) ON accounts TO finance_edit;

UPDATE accounts SET retirement = true
WHERE coalesce(display_name, name) ~* '(\m401\s*\(?k|\m403\s*\(?b|\mira\M|\mroth\M|retire|pension|rollover)';
