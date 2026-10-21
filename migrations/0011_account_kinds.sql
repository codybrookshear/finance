-- Account kinds for the Accounts page's chart selector: checking, savings,
-- brokerage (kind 'investment'), real estate, and retirement (the separate
-- `retirement` flag, which wins over kind). A user field like retirement:
-- sync never writes it. The first guess is by name, for accounts with no kind
-- yet; fix any it got wrong with
--   UPDATE accounts SET kind = '<checking|savings|investment|real_estate|credit|loan|other>' WHERE id = '...'
-- (as finance_owner, or finance_edit). Accounts of no listed kind count only
-- in the total.
ALTER TABLE accounts DROP CONSTRAINT accounts_kind_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_kind_check
    CHECK (kind IN ('checking','savings','credit','investment','loan','real_estate','other'));
GRANT UPDATE (kind) ON accounts TO finance_edit;

UPDATE accounts SET kind = CASE
    WHEN id LIKE 'manual:%' AND (org_name ~* 'real estate' OR name ~* '\m(home|house|property|rental)\M') THEN 'real_estate'
    WHEN coalesce(display_name, name) ~* 'saving' THEN 'savings'
    WHEN coalesce(display_name, name) ~* 'check' THEN 'checking'
    WHEN org_name || ' ' || coalesce(display_name, name) ~* '(brokerage|invest|robinhood|schwab|fidelity|vanguard|e\*?trade|wealthfront|betterment)' THEN 'investment'
END
WHERE kind IS NULL;
