-- How categories look in the UI: an emoji, and a hue (0-359) for the chip's
-- background. NULL hue = neutral gray.

ALTER TABLE categories
    ADD COLUMN emoji text NOT NULL DEFAULT '🏷️',
    ADD COLUMN hue integer CHECK (hue BETWEEN 0 AND 359);

UPDATE categories c SET emoji = v.emoji, hue = v.hue
FROM (VALUES
    ('Home',          '🏠',  28),
    ('Shopping',      '🛍️', 285),
    ('Kids',          '🧸', 330),
    ('Health',        '🩺', 355),
    ('Education',     '🎓', 225),
    ('Dining',        '🍽️',  12),
    ('Auto',          '🚗', 205),
    ('Groceries',     '🛒', 115),
    ('Entertainment', '🎬', 260),
    ('Travel',        '✈️', 185),
    ('Personal Care', '💅', 310),
    ('Pets',          '🐾',  40),
    ('Utilities',     '💡',  55),
    ('Fitness',       '🏋️', 160),
    ('Other',         '📦', NULL),
    ('Income',        '💰', 135),
    ('Transfer',      '🔁', NULL)
) AS v(name, emoji, hue)
WHERE c.name = v.name;
