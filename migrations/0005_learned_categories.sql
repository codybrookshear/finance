-- Learn categories from your own choices, instead of rules.
--
-- A transaction you haven't categorized gets the category you gave the most
-- similar transactions: similar merchant (trigram similarity of a normalized
-- payee/description), money moving the same way. Each of your choices votes
-- with weight similarity² × ½^(age of the choice / 90 days), so recent
-- choices count more and re-categorizing shifts later guesses. It's only
-- applied when one category gets at least 60% of the votes. Transfer-pair
-- detection stays. Anything you (or Claude) set by hand is never changed.
--
-- categorizations records every category set or changed by hand: the history
-- the recency weighting uses, and a record of which guesses you corrected.

ALTER ROLE finance_rules RENAME TO finance_categorizer;

DROP FUNCTION categorize();
UPDATE transactions SET category_id = NULL, is_transfer = false, category_source = NULL
WHERE category_source = 'rule';
DROP TABLE rules;

ALTER TABLE transactions DROP CONSTRAINT transactions_category_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_category_source_check
    CHECK (category_source IN ('learned', 'auto', 'manual', 'claude'));

-- The text compared between transactions: payee if the bank gave one (usually
-- a clean merchant name), else the description; lowercased, letters only, so
-- store numbers, dates and card digits don't matter.
CREATE FUNCTION merchant_key(payee text, description text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN btrim(regexp_replace(lower(coalesce(nullif(btrim(payee), ''), description, '')),
                            '[^[:alpha:]]+', ' ', 'g'));

ALTER TABLE transactions
    ADD COLUMN merchant text GENERATED ALWAYS AS (merchant_key(payee, description)) STORED;
CREATE INDEX transactions_merchant_trgm ON transactions USING gin (merchant gin_trgm_ops);

CREATE TABLE categorizations (
    id                   bigserial PRIMARY KEY,
    account_id           text NOT NULL,
    transaction_id       text NOT NULL,
    category_id          integer REFERENCES categories(id),  -- NULL: set back to automatic
    source               text NOT NULL CHECK (source IN ('manual', 'claude')),
    previous_category_id integer REFERENCES categories(id),
    previous_source      text,              -- e.g. 'learned': you corrected a guess
    categorized_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (account_id, transaction_id) REFERENCES transactions (account_id, id) ON DELETE CASCADE
);
CREATE INDEX categorizations_by_txn ON categorizations (account_id, transaction_id, categorized_at DESC);

-- Logged in the database, so every writer (web UI, a future MCP tool) is
-- recorded the same way.
CREATE FUNCTION log_categorization() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    INSERT INTO public.categorizations
        (account_id, transaction_id, category_id, source, previous_category_id, previous_source)
    VALUES (NEW.account_id, NEW.id,
            CASE WHEN NEW.category_source IN ('manual', 'claude') THEN NEW.category_id END,
            coalesce(NEW.category_source, OLD.category_source),
            OLD.category_id, OLD.category_source);
    RETURN NULL;
END $$;

CREATE TRIGGER log_categorization
AFTER UPDATE OF category_id, category_source ON transactions
FOR EACH ROW
WHEN ((NEW.category_source IN ('manual', 'claude') OR OLD.category_source IN ('manual', 'claude'))
      AND (OLD.category_id IS DISTINCT FROM NEW.category_id
           OR OLD.category_source IS DISTINCT FROM NEW.category_source))
EXECUTE FUNCTION log_categorization();

-- categorize(): with no argument (after each sync) it fills in transactions
-- that have no category yet. With a merchant (after you categorize one) it
-- re-guesses everything similar to that merchant that isn't set by hand,
-- including earlier guesses, and clears guesses that no longer have support.
-- Returns how many it guessed, cleared, and marked as transfers.
CREATE FUNCTION categorize(p_merchant text DEFAULT NULL,
                           OUT learned integer, OUT cleared integer, OUT transfers integer)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = public, pg_temp
SET pg_trgm.similarity_threshold = 0.5
AS $$
DECLARE
    half_life constant double precision := 90 * 86400;  -- seconds
    min_share constant double precision := 0.6;          -- winner's share of the votes
    transfer_category integer :=
        (SELECT id FROM public.categories WHERE kind = 'transfer' ORDER BY id LIMIT 1);
BEGIN
    -- Transfers between your own accounts first: an outflow and an inflow of
    -- the same amount, in different accounts, within 4 days, both posted and
    -- uncategorized, where each has exactly one such counterpart.
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

    WITH targets AS (
        SELECT t.account_id, t.id, t.merchant, t.amount
        FROM public.transactions t
        WHERE t.merchant <> ''
          AND CASE WHEN p_merchant IS NULL THEN t.category_source IS NULL
                   ELSE (t.category_source IS NULL OR t.category_source = 'learned')
                        AND t.merchant % p_merchant END
    ),
    votes AS (
        SELECT g.account_id, g.id, l.category_id,
               sum(power(similarity(g.merchant, l.merchant), 2)
                   * power(0.5, extract(epoch FROM now() - l.chosen_at) / half_life)) AS weight
        FROM targets g
        JOIN LATERAL (
            SELECT s.merchant, s.category_id,
                   coalesce((SELECT max(c.categorized_at) FROM public.categorizations c
                             WHERE c.account_id = s.account_id AND c.transaction_id = s.id),
                            s.first_seen_at) AS chosen_at
            FROM public.transactions s
            WHERE s.merchant % g.merchant
              AND s.category_source IN ('manual', 'claude')
              AND s.category_id IS NOT NULL
              AND sign(s.amount) = sign(g.amount)
              AND (s.account_id, s.id) <> (g.account_id, g.id)
        ) l ON true
        GROUP BY g.account_id, g.id, l.category_id
    ),
    decided AS (
        SELECT account_id, id, category_id FROM (
            SELECT v.*,
                   row_number() OVER (PARTITION BY account_id, id ORDER BY weight DESC, category_id) AS rank,
                   sum(weight) OVER (PARTITION BY account_id, id) AS total
            FROM votes v
        ) r
        WHERE rank = 1 AND weight >= min_share * total
    ),
    changed AS (
        UPDATE public.transactions t
        SET category_id = d.category_id,
            is_transfer = coalesce(d.category_id = transfer_category, false),
            category_source = CASE WHEN d.category_id IS NULL THEN NULL ELSE 'learned' END
        FROM targets g
        LEFT JOIN decided d ON d.account_id = g.account_id AND d.id = g.id
        WHERE t.account_id = g.account_id AND t.id = g.id
          AND (t.category_id IS DISTINCT FROM d.category_id
               OR t.category_source IS DISTINCT FROM
                  CASE WHEN d.category_id IS NULL THEN NULL ELSE 'learned' END)
        RETURNING t.category_source
    )
    SELECT count(*) FILTER (WHERE category_source = 'learned'),
           count(*) FILTER (WHERE category_source IS NULL)
    INTO learned, cleared
    FROM changed;
END $$;

-- finance_categorizer owns both functions: it can read, change category
-- columns, and append to the history. Nothing else.
GRANT SELECT ON categorizations TO finance_categorizer;
GRANT INSERT ON categorizations TO finance_categorizer;
GRANT USAGE ON SEQUENCE categorizations_id_seq TO finance_categorizer;
ALTER FUNCTION categorize(text) OWNER TO finance_categorizer;
ALTER FUNCTION log_categorization() OWNER TO finance_categorizer;
REVOKE ALL ON FUNCTION categorize(text), log_categorization() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION categorize(text) TO finance_sync, finance_edit;

GRANT SELECT ON categorizations TO finance_web, finance_edit;
