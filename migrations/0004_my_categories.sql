-- Categories as I actually budget. Income and Transfer (moves between my own
-- accounts and credit-card payments; excluded from spending) stay as they are.
-- More can be added later.

UPDATE categories SET name = 'Home' WHERE name = 'Housing';
UPDATE categories SET name = 'Auto' WHERE name = 'Transportation';

INSERT INTO categories (name, kind) VALUES
    ('Kids', 'expense'), ('Education', 'expense'), ('Personal Care', 'expense'),
    ('Pets', 'expense'), ('Fitness', 'expense')
ON CONFLICT (name) DO NOTHING;

-- Defaults I don't use, unless something already refers to them.
DELETE FROM categories c
WHERE c.name IN ('Subscriptions', 'Fees')
  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.category_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM rules r WHERE r.category_id = c.id);
