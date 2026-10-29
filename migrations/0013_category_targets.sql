-- Monthly spending targets per expense category, set by hand on the Spending
-- page. A category without a row has no target.
CREATE TABLE category_targets (
    category_id integer PRIMARY KEY REFERENCES categories(id) ON DELETE CASCADE,
    amount      numeric NOT NULL CHECK (amount >= 0)
);
GRANT SELECT ON category_targets TO finance_web;
GRANT SELECT, INSERT, UPDATE, DELETE ON category_targets TO finance_edit;
