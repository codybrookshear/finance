package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"finance/internal/simplefin"
	"finance/internal/store"
)

// Integration tests. They need a migrated database and connect as the
// finance_sync role, so they also prove its grants are sufficient:
//
//	TEST_DATABASE_URL=postgres://finance_sync@127.0.0.1:5432/finance \
//	TEST_OWNER_DATABASE_URL=postgres://finance_owner@127.0.0.1:5432/finance go test ./...

type fakeBank struct {
	balance string
	txns    []simplefin.Transaction
	queries []simplefin.Query
	// A second bank, connected later: nil until then.
	savings []simplefin.Transaction
}

// inWindow keeps the transactions a query asks for.
func inWindow(txns []simplefin.Transaction, q simplefin.Query) []simplefin.Transaction {
	var out []simplefin.Transaction
	for _, t := range txns {
		if t.Pending && !q.Pending {
			continue
		}
		ts := t.Posted
		if ts.IsZero() && t.TransactedAt != nil {
			ts = *t.TransactedAt
		}
		tt := ts.Time()
		if (q.Start.IsZero() || !tt.Before(q.Start)) && (q.End.IsZero() || tt.Before(q.End)) {
			out = append(out, t)
		}
	}
	return out
}

func (f *fakeBank) Accounts(_ context.Context, q simplefin.Query) (*simplefin.AccountSet, error) {
	f.queries = append(f.queries, q)
	set := &simplefin.AccountSet{
		Connections: []simplefin.Connection{{ConnID: "C1", Name: "Example Bank", OrgURL: "https://bank.example"}},
		Accounts: []simplefin.Account{{
			ID: "A1", Name: "Checking", ConnID: "C1", Currency: "USD",
			Balance: simplefin.Decimal(f.balance), BalanceDate: simplefin.UnixTime(f.now().Unix()),
			Transactions: inWindow(f.txns, q),
		}},
	}
	if f.savings != nil {
		set.Connections = append(set.Connections, simplefin.Connection{ConnID: "C2", Name: "Second Bank"})
		set.Accounts = append(set.Accounts, simplefin.Account{
			ID: "S1", Name: "Savings", ConnID: "C2", Currency: "USD",
			Balance: "500.00", BalanceDate: simplefin.UnixTime(f.now().Unix()),
			Transactions: inWindow(f.savings, q),
		})
	}
	return set, nil
}

var clock time.Time

func (f *fakeBank) now() time.Time { return clock }

func ut(t time.Time) simplefin.UnixTime { return simplefin.UnixTime(t.Unix()) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	owner := ownerPool(t) // the sync role (correctly) can't delete accounts
	for _, q := range []string{
		`DELETE FROM balance_snapshots`, `DELETE FROM transactions`, `DELETE FROM accounts`,
		`DELETE FROM connections`, `DELETE FROM sync_runs`,
		`UPDATE sync_state SET last_success_at=NULL, backfill_before=NULL, backfill_empty_runs=0, backfill_done=false`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return pool
}

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_OWNER_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_OWNER_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestSyncEndToEnd(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	la, _ := time.LoadLocation("America/Los_Angeles")

	day := 24 * time.Hour
	clock = time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	t0 := clock

	bank := &fakeBank{balance: "1000.00"}
	// One transaction every 10 days going back 250 days.
	for i := 1; i <= 25; i++ {
		ts := t0.Add(-time.Duration(i*10) * day)
		bank.txns = append(bank.txns, simplefin.Transaction{
			ID: "T" + string(rune('A'+i)), Posted: ut(ts), Amount: "-10.00", Description: "STORE #" + string(rune('A'+i)),
		})
	}
	pendingAt := ut(t0.Add(-1 * day))
	bank.txns = append(bank.txns, simplefin.Transaction{
		ID: "P1", Pending: true, Amount: "-4.50", Description: "COFFEE (pending)", TransactedAt: &pendingAt,
	})

	cfg := DefaultConfig()
	cfg.Location = la
	cfg.Now = func() time.Time { return clock }
	cfg.MaxLookback = 400 * day
	cfg.Window = 89 * day     // the scenario below is sized for 89-day windows
	cfg.MaxBackfillPerRun = 3 // ... and 3 of them per run, whatever the defaults
	s := &Syncer{Store: &store.Store{Pool: pool}, Client: bank, Cfg: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// --- Run 1: incremental + 3 backfill windows ---
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 4 {
		t.Errorf("run1 requests = %d, want 4", res.Requests)
	}
	if !bank.queries[0].Pending || bank.queries[1].Pending {
		t.Errorf("pending flags wrong: %+v", bank.queries)
	}
	if n := count(t, pool, `SELECT count(*) FROM transactions`); n != 26 {
		t.Errorf("run1 txns = %d, want 26 (all 25 within 3 windows + pending)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM transactions WHERE pending AND id='P1' AND posted_at IS NULL`); n != 1 {
		t.Errorf("pending txn not stored as pending")
	}
	if n := count(t, pool, `SELECT count(*) FROM balance_snapshots WHERE as_of = '2026-10-02' AND balance = 1000`); n != 1 {
		t.Errorf("snapshot for local date missing")
	}

	// Column-level grants: the sync role cannot touch categorization.
	if _, err := pool.Exec(ctx, `UPDATE transactions SET category_source='manual' WHERE id='TB'`); err == nil {
		t.Errorf("finance_sync was able to edit categorization columns")
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET hidden = true`); err == nil {
		t.Errorf("finance_sync was able to edit user account fields")
	}

	// --- Run 2, next day: pending posts under a new ID, balance changes ---
	clock = t0.Add(day)
	bank.balance = "995.50"
	bank.txns = bank.txns[:len(bank.txns)-1]
	bank.txns = append(bank.txns, simplefin.Transaction{
		ID: "P1-POSTED", Posted: ut(t0), Amount: "-4.50", Description: "COFFEE",
	})
	bank.queries = nil
	res, err = s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := bank.queries[0].Start; !got.Equal(t0.Add(-5 * day)) {
		t.Errorf("incremental start = %v, want last success - 5d", got)
	}
	if res.StaleDeleted != 1 {
		t.Errorf("stale pending deleted = %d, want 1", res.StaleDeleted)
	}
	if n := count(t, pool, `SELECT count(*) FROM transactions WHERE id='P1'`); n != 0 {
		t.Errorf("stale pending P1 still present")
	}
	if n := count(t, pool, `SELECT count(*) FROM transactions WHERE id='P1-POSTED' AND NOT pending`); n != 1 {
		t.Errorf("posted txn missing")
	}
	if n := count(t, pool, `SELECT count(*) FROM balance_snapshots`); n != 2 {
		t.Errorf("snapshots = %d, want 2 (one per day)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM accounts WHERE balance = 995.50`); n != 1 {
		t.Errorf("account balance not updated")
	}

	// Keep running until backfill completes; it must stop on its own.
	for i := 0; i < 10; i++ {
		clock = clock.Add(6 * time.Hour)
		if _, err := s.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Store.State(ctx)
	if !st.BackfillDone {
		t.Errorf("backfill never completed: %+v", st)
	}
	if n := count(t, pool, `SELECT count(*) FROM sync_runs WHERE status='ok'`); n != 12 {
		t.Errorf("ok runs = %d, want 12", n)
	}
	// Each run stays well within SimpleFIN's ~24 requests/day guidance.
	if n := count(t, pool, `SELECT max(requests) FROM sync_runs`); n > 4 {
		t.Errorf("max requests per run = %d", n)
	}
}

func TestSyncPreservesUserFields(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	clock = time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	bank := &fakeBank{balance: "1.00", txns: []simplefin.Transaction{
		{ID: "X", Posted: ut(clock.Add(-time.Hour)), Amount: "-1.00", Description: "ORIGINAL"},
	}}
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return clock }
	cfg.MaxBackfillPerRun = 0
	s := &Syncer{Store: &store.Store{Pool: pool}, Client: bank, Cfg: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate user edits as the schema owner (the sync role can't).
	owner := ownerPool(t)
	if _, err := owner.Exec(ctx, `UPDATE transactions SET is_transfer = true, note = 'n' WHERE id = 'X'`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE accounts SET display_name = 'Joint', kind = 'checking' WHERE id = 'A1'`); err != nil {
		t.Fatal(err)
	}

	bank.txns[0].Description = "UPDATED BY BANK"
	clock = clock.Add(time.Hour)
	if _, err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM transactions WHERE id='X' AND is_transfer AND note='n' AND description='UPDATED BY BANK'`); n != 1 {
		t.Error("sync overwrote user transaction fields or missed bank update")
	}
	if n := count(t, pool, `SELECT count(*) FROM accounts WHERE display_name='Joint' AND kind='checking'`); n != 1 {
		t.Error("sync overwrote user account fields")
	}
}

// A bank connected after the backfill finished still gets its history.
func TestNewAccountGetsHistory(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	day := 24 * time.Hour
	clock = time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)

	bank := &fakeBank{balance: "100.00", txns: []simplefin.Transaction{
		{ID: "R1", Posted: ut(clock.Add(-2 * day)), Amount: "-1.00", Description: "RECENT"},
	}}
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return clock }
	cfg.MaxLookback = 400 * day
	cfg.MaxBackfillPerRun = 3 // enough to reach 4 months back in one run
	s := &Syncer{Store: &store.Store{Pool: pool}, Client: bank, Cfg: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Sync until the backfill is done (empty windows end it).
	for i := 0; i < 5; i++ {
		if _, err := s.Run(ctx); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(6 * time.Hour)
	}
	if n := count(t, pool, `SELECT count(*) FROM sync_state WHERE backfill_done`); n != 1 {
		t.Fatal("backfill should be done before the second bank appears")
	}

	// Connect a second bank with history going back four months.
	for i, age := range []int{10, 60, 120} {
		bank.savings = append(bank.savings, simplefin.Transaction{
			ID: fmt.Sprintf("S%d", i), Posted: ut(clock.Add(-time.Duration(age) * day)), Amount: "25.00", Description: "DEPOSIT",
		})
	}
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewAccounts != 1 {
		t.Errorf("new accounts = %d, want 1", res.NewAccounts)
	}
	if n := count(t, pool, `SELECT count(*) FROM transactions WHERE account_id = 'S1'`); n != 3 {
		t.Errorf("second bank has %d transactions after one sync, want all 3", n)
	}
}
