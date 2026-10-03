-- Core schema. Money is NUMERIC everywhere; never float.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE connections (
    id          text PRIMARY KEY,
    name        text NOT NULL DEFAULT '',
    org_url     text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE accounts (
    id                    text PRIMARY KEY,
    connection_id         text REFERENCES connections(id),
    org_name              text NOT NULL DEFAULT '',
    name                  text NOT NULL,
    currency              text NOT NULL DEFAULT 'USD',
    -- User-maintained fields (never overwritten by sync):
    display_name          text,
    kind                  text CHECK (kind IN ('checking','savings','credit','investment','loan','other')),
    include_in_net_worth  boolean NOT NULL DEFAULT true,
    hidden                boolean NOT NULL DEFAULT false,
    -- Latest values from SimpleFIN:
    balance               numeric NOT NULL DEFAULT 0,
    available_balance     numeric,
    balance_at            timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE categories (
    id         serial PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    kind       text NOT NULL DEFAULT 'expense' CHECK (kind IN ('expense','income','transfer')),
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO categories (name, kind) VALUES
    ('Groceries','expense'), ('Dining','expense'), ('Housing','expense'),
    ('Utilities','expense'), ('Transportation','expense'), ('Shopping','expense'),
    ('Health','expense'), ('Entertainment','expense'), ('Travel','expense'),
    ('Subscriptions','expense'), ('Fees','expense'), ('Other','expense'),
    ('Income','income'), ('Transfer','transfer');

CREATE TABLE transactions (
    account_id      text NOT NULL REFERENCES accounts(id),
    id              text NOT NULL,
    posted_at       timestamptz,            -- NULL while pending (SimpleFIN sends 0)
    transacted_at   timestamptz,
    amount          numeric NOT NULL,       -- negative = money out
    description     text NOT NULL DEFAULT '',
    payee           text NOT NULL DEFAULT '',
    memo            text NOT NULL DEFAULT '',
    pending         boolean NOT NULL DEFAULT false,
    -- Categorization (sync never overwrites these):
    category_id     integer REFERENCES categories(id),
    category_source text CHECK (category_source IN ('rule','manual','claude')),
    is_transfer     boolean NOT NULL DEFAULT false,
    note            text,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    search          tsvector GENERATED ALWAYS AS (
                        to_tsvector('simple', description || ' ' || payee || ' ' || memo)
                    ) STORED,
    PRIMARY KEY (account_id, id)
);

CREATE INDEX transactions_date_idx   ON transactions (coalesce(transacted_at, posted_at) DESC);
CREATE INDEX transactions_search_idx ON transactions USING gin (search);
CREATE INDEX transactions_desc_trgm  ON transactions USING gin (description gin_trgm_ops);
CREATE INDEX transactions_pending_idx ON transactions (account_id) WHERE pending;

-- One row per account per local calendar day; later syncs that day overwrite.
CREATE TABLE balance_snapshots (
    account_id        text NOT NULL REFERENCES accounts(id),
    as_of             date NOT NULL,
    balance           numeric NOT NULL,
    available_balance numeric,
    balance_at        timestamptz NOT NULL,
    PRIMARY KEY (account_id, as_of)
);

CREATE TABLE rules (
    id          serial PRIMARY KEY,
    pattern     text NOT NULL,        -- case-insensitive substring match
    field       text NOT NULL DEFAULT 'description' CHECK (field IN ('description','payee','memo')),
    category_id integer REFERENCES categories(id),
    is_transfer boolean NOT NULL DEFAULT false,
    priority    integer NOT NULL DEFAULT 100,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sync_runs (
    id                bigserial PRIMARY KEY,
    started_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,
    status            text NOT NULL DEFAULT 'running' CHECK (status IN ('running','ok','error')),
    requests          integer NOT NULL DEFAULT 0,
    txns_seen         integer NOT NULL DEFAULT 0,
    message           text NOT NULL DEFAULT ''
);

-- Singleton row tracking incremental and backfill progress.
CREATE TABLE sync_state (
    id                  integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    last_success_at     timestamptz,
    backfill_before     timestamptz,   -- next backfill window ends here
    backfill_empty_runs integer NOT NULL DEFAULT 0,
    backfill_done       boolean NOT NULL DEFAULT false
);
INSERT INTO sync_state (id) VALUES (1);
