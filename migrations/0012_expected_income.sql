-- Expected monthly income, set by hand on the Spending page. One row at most;
-- with none, the app expects last month's income.
CREATE TABLE expected_income (
    id     integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    amount numeric NOT NULL CHECK (amount >= 0)
);
GRANT SELECT ON expected_income TO finance_web;
GRANT SELECT, INSERT, UPDATE, DELETE ON expected_income TO finance_edit;
