// Package store holds the database access used by the sync job.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ Pool *pgxpool.Pool }

// SyncState mirrors the sync_state singleton row.
type SyncState struct {
	LastSuccessAt     *time.Time
	BackfillBefore    *time.Time
	BackfillEmptyRuns int
	BackfillDone      bool
}

func (s *Store) State(ctx context.Context) (SyncState, error) {
	var st SyncState
	err := s.Pool.QueryRow(ctx, `SELECT last_success_at, backfill_before, backfill_empty_runs, backfill_done
		FROM sync_state WHERE id = 1`).Scan(&st.LastSuccessAt, &st.BackfillBefore, &st.BackfillEmptyRuns, &st.BackfillDone)
	return st, err
}

func (s *Store) SaveState(ctx context.Context, st SyncState) error {
	_, err := s.Pool.Exec(ctx, `UPDATE sync_state SET last_success_at = $1, backfill_before = $2,
		backfill_empty_runs = $3, backfill_done = $4 WHERE id = 1`,
		st.LastSuccessAt, st.BackfillBefore, st.BackfillEmptyRuns, st.BackfillDone)
	return err
}

func (s *Store) StartRun(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO sync_runs DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

func (s *Store) FinishRun(ctx context.Context, id int64, ok bool, requests, txns int, msg string) error {
	status := "ok"
	if !ok {
		status = "error"
	}
	_, err := s.Pool.Exec(ctx, `UPDATE sync_runs SET finished_at = now(), status = $2,
		requests = $3, txns_seen = $4, message = $5 WHERE id = $1`, id, status, requests, txns, msg)
	return err
}

// Tx runs fn in a transaction.
func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, fn)
}

type Connection struct{ ID, Name, OrgURL string }

func UpsertConnection(ctx context.Context, tx pgx.Tx, c Connection) error {
	_, err := tx.Exec(ctx, `INSERT INTO connections (id, name, org_url) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, org_url = EXCLUDED.org_url, updated_at = now()`,
		c.ID, c.Name, c.OrgURL)
	return err
}

type Account struct {
	ID, ConnectionID, OrgName, Name, Currency string
	Balance                                   string
	AvailableBalance                          *string
	BalanceAt                                 time.Time
	UpdateBalance                             bool // false during backfill-only fetches
}

// UpsertAccount reports whether the account is new (inserted, not updated).
func UpsertAccount(ctx context.Context, tx pgx.Tx, a Account) (inserted bool, err error) {
	var connID *string
	if a.ConnectionID != "" {
		connID = &a.ConnectionID
	}
	// Only sync-owned columns are updated; user fields (display_name, kind, ...) are left alone.
	err = tx.QueryRow(ctx, `INSERT INTO accounts (id, connection_id, org_name, name, currency, balance, available_balance, balance_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8)
		ON CONFLICT (id) DO UPDATE SET
			connection_id = EXCLUDED.connection_id, org_name = EXCLUDED.org_name,
			name = EXCLUDED.name, currency = EXCLUDED.currency,
			balance           = CASE WHEN $9 THEN EXCLUDED.balance ELSE accounts.balance END,
			available_balance = CASE WHEN $9 THEN EXCLUDED.available_balance ELSE accounts.available_balance END,
			balance_at        = CASE WHEN $9 THEN EXCLUDED.balance_at ELSE accounts.balance_at END,
			updated_at = now()
		RETURNING xmax = 0`,
		a.ID, connID, a.OrgName, a.Name, a.Currency, a.Balance, a.AvailableBalance, a.BalanceAt, a.UpdateBalance).
		Scan(&inserted)
	return inserted, err
}

type Transaction struct {
	AccountID, ID            string
	PostedAt, TransactedAt   *time.Time
	Amount                   string
	Description, Payee, Memo string
	Pending                  bool
}

func UpsertTransaction(ctx context.Context, tx pgx.Tx, t Transaction) error {
	// Categorization columns are never touched by sync.
	_, err := tx.Exec(ctx, `INSERT INTO transactions
			(account_id, id, posted_at, transacted_at, amount, description, payee, memo, pending)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8,$9)
		ON CONFLICT (account_id, id) DO UPDATE SET
			posted_at = EXCLUDED.posted_at, transacted_at = EXCLUDED.transacted_at,
			amount = EXCLUDED.amount, description = EXCLUDED.description,
			payee = EXCLUDED.payee, memo = EXCLUDED.memo, pending = EXCLUDED.pending,
			updated_at = now()
		WHERE (transactions.posted_at, transactions.transacted_at, transactions.amount,
		       transactions.description, transactions.payee, transactions.memo, transactions.pending)
		  IS DISTINCT FROM
		      (EXCLUDED.posted_at, EXCLUDED.transacted_at, EXCLUDED.amount,
		       EXCLUDED.description, EXCLUDED.payee, EXCLUDED.memo, EXCLUDED.pending)`,
		t.AccountID, t.ID, t.PostedAt, t.TransactedAt, t.Amount, t.Description, t.Payee, t.Memo, t.Pending)
	return err
}

// DeleteStalePending removes pending transactions for an account that the
// server no longer reports (they posted under a new ID, or were voided).
// Only call this with the complete set of IDs from a fetch that included
// pending transactions and covered every date a pending txn could have.
func DeleteStalePending(ctx context.Context, tx pgx.Tx, accountID string, seenIDs []string) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM transactions
		WHERE account_id = $1 AND pending AND NOT (id = ANY($2))`, accountID, seenIDs)
	return tag.RowsAffected(), err
}

func UpsertSnapshot(ctx context.Context, tx pgx.Tx, accountID string, asOf time.Time, balance string, available *string, balanceAt time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO balance_snapshots (account_id, as_of, balance, available_balance, balance_at)
		VALUES ($1, $2::date, $3::numeric, $4::numeric, $5)
		ON CONFLICT (account_id, as_of) DO UPDATE SET
			balance = EXCLUDED.balance, available_balance = EXCLUDED.available_balance,
			balance_at = EXCLUDED.balance_at
		WHERE EXCLUDED.balance_at >= balance_snapshots.balance_at`,
		accountID, asOf.Format("2006-01-02"), balance, available, balanceAt)
	return err
}

// Categorized counts what categorize() changed.
type Categorized struct{ Learned, Cleared, Transfers int }

// Categorize runs the database's categorize() (migrations/0005) on
// transactions without a category: transfer detection, then guesses learned
// from the categories you set by hand. It never changes those. The sync role
// can run it but can't write category columns itself.
func (s *Store) Categorize(ctx context.Context) (Categorized, error) {
	var c Categorized
	err := s.Pool.QueryRow(ctx, `SELECT learned, cleared, transfers FROM categorize()`).
		Scan(&c.Learned, &c.Cleared, &c.Transfers)
	return c, err
}
