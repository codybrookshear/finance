-- Categorization: rules, manual edits from the web UI, and automatic transfer
-- detection.
--
-- New roles (NOLOGIN, so no passwords):
--   finance_rules: owns categorize(), the only code that sets rule-based and
--                  detected categories. It can change category columns and
--                  nothing else.
--   finance_edit : what the web UI's edit requests run as. finance_web may
--                  SET ROLE to it but does not inherit it, so ordinary
--                  finance_web sessions (and the future read-only API) stay
--                  read-only.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'finance_rules') THEN
        CREATE ROLE finance_rules NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'finance_edit') THEN
        CREATE ROLE finance_edit NOLOGIN;
    END IF;
END $$;

-- 'auto' marks a transfer found by detection (below).
ALTER TABLE transactions DROP CONSTRAINT transactions_category_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_category_source_check
    CHECK (category_source IN ('rule', 'auto', 'manual', 'claude'));

ALTER TABLE rules
    ADD CONSTRAINT rules_pattern_length CHECK (length(pattern) BETWEEN 1 AND 200),
    ADD CONSTRAINT rules_does_something CHECK (category_id IS NOT NULL OR is_transfer);
CREATE UNIQUE INDEX rules_unique_pattern ON rules (field, lower(pattern));

-- categorize() brings automatic categories up to date and returns how many
-- transactions it changed. It never touches a transaction whose category was
-- set by hand ('manual') or by Claude.
--   1. Rules: each uncategorized or rule-categorized transaction gets the
--      first rule (by priority, then id) whose pattern appears in its field,
--      case-insensitively.
--   2. Rule categories whose rule no longer matches (deleted or edited rule)
--      go back to uncategorized.
--   3. Transfers between your own accounts: an outflow and an inflow of the
--      same amount, in different accounts, within 4 days, both posted and
--      untouched, where each has exactly one such counterpart. Ambiguous
--      cases are left for you.
CREATE FUNCTION categorize(OUT ruled integer, OUT cleared integer, OUT transfers integer)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    transfer_category integer :=
        (SELECT id FROM public.categories WHERE kind = 'transfer' ORDER BY id LIMIT 1);
BEGIN
    WITH matched AS (
        SELECT DISTINCT ON (t.account_id, t.id) t.account_id, t.id, r.category_id, r.is_transfer
        FROM public.transactions t
        JOIN public.rules r ON strpos(lower(CASE r.field WHEN 'payee' THEN t.payee
                                                         WHEN 'memo' THEN t.memo
                                                         ELSE t.description END),
                                      lower(r.pattern)) > 0
        WHERE t.category_source IS NULL OR t.category_source = 'rule'
        ORDER BY t.account_id, t.id, r.priority, r.id
    )
    UPDATE public.transactions t
    SET category_id = m.category_id, is_transfer = m.is_transfer, category_source = 'rule'
    FROM matched m
    WHERE t.account_id = m.account_id AND t.id = m.id
      AND (t.category_source IS DISTINCT FROM 'rule'
           OR t.category_id IS DISTINCT FROM m.category_id
           OR t.is_transfer IS DISTINCT FROM m.is_transfer);
    GET DIAGNOSTICS ruled = ROW_COUNT;

    UPDATE public.transactions t
    SET category_id = NULL, is_transfer = false, category_source = NULL
    WHERE t.category_source = 'rule'
      AND NOT EXISTS (
          SELECT 1 FROM public.rules r
          WHERE strpos(lower(CASE r.field WHEN 'payee' THEN t.payee
                                          WHEN 'memo' THEN t.memo
                                          ELSE t.description END),
                       lower(r.pattern)) > 0);
    GET DIAGNOSTICS cleared = ROW_COUNT;

    WITH candidates AS (
        SELECT o.account_id AS out_account, o.id AS out_id, i.account_id AS in_account, i.id AS in_id
        FROM public.transactions o
        JOIN public.transactions i
          ON i.amount = -o.amount
         AND i.account_id <> o.account_id
         AND abs(extract(epoch FROM coalesce(i.transacted_at, i.posted_at)
                                  - coalesce(o.transacted_at, o.posted_at))) <= 4 * 86400
        WHERE o.amount < 0
          AND NOT o.pending AND NOT i.pending
          AND o.category_source IS NULL AND i.category_source IS NULL
    ),
    pairs AS (
        SELECT * FROM (
            SELECT c.*,
                   count(*) OVER (PARTITION BY out_account, out_id) AS outs,
                   count(*) OVER (PARTITION BY in_account, in_id) AS ins
            FROM candidates c
        ) x
        WHERE outs = 1 AND ins = 1
    )
    UPDATE public.transactions t
    SET is_transfer = true, category_id = transfer_category, category_source = 'auto'
    FROM pairs p
    WHERE (t.account_id = p.out_account AND t.id = p.out_id)
       OR (t.account_id = p.in_account AND t.id = p.in_id);
    GET DIAGNOSTICS transfers = ROW_COUNT;
END $$;

GRANT USAGE ON SCHEMA public TO finance_rules, finance_edit;

-- finance_rules: just enough for categorize().
GRANT SELECT ON transactions, rules, categories TO finance_rules;
GRANT UPDATE (category_id, category_source, is_transfer) ON transactions TO finance_rules;
ALTER FUNCTION categorize() OWNER TO finance_rules;
REVOKE ALL ON FUNCTION categorize() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION categorize() TO finance_sync, finance_edit;

-- finance_edit: read everything, change only the user fields and the rules.
GRANT SELECT ON ALL TABLES IN SCHEMA public TO finance_edit;
REVOKE ALL ON schema_migrations FROM finance_edit;
GRANT UPDATE (category_id, category_source, is_transfer, note) ON transactions TO finance_edit;
GRANT SELECT, INSERT, UPDATE, DELETE ON rules TO finance_edit;
GRANT USAGE ON SEQUENCE rules_id_seq TO finance_edit;

GRANT finance_edit TO finance_web WITH INHERIT FALSE, SET TRUE;
