package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration test: needs a migrated database (make test). Categorize runs as
// finance_sync, which also proves that role can run categorize() but can't
// write category columns itself.
func TestCategorize(t *testing.T) {
	syncURL, ownerURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_OWNER_DATABASE_URL")
	if syncURL == "" || ownerURL == "" {
		t.Skip("TEST_DATABASE_URL / TEST_OWNER_DATABASE_URL not set")
	}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	syncPool, err := pgxpool.New(ctx, syncURL)
	if err != nil {
		t.Fatal(err)
	}
	defer syncPool.Close()
	st := &Store{Pool: syncPool}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, s := range []string{`DELETE FROM balance_snapshots`, `DELETE FROM transactions`, `DELETE FROM rules`,
		`DELETE FROM accounts`, `DELETE FROM connections`} {
		exec(s)
	}
	exec(`INSERT INTO accounts (id, name) VALUES ('chk', 'Checking'), ('card', 'Card'), ('sav', 'Savings')`)

	d := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	type txn struct {
		acct, id, amount, desc, payee string
		at                            time.Time
		pending                       bool
	}
	for _, x := range []txn{
		{"chk", "groc", "-40.00", "GROCERY OUTLET #12", "", d, false},
		{"card", "both", "-9.99", "NETFLIX GROCERY", "Netflix", d, false}, // two rules match: priority decides
		{"card", "flix", "-15.49", "NETFLIX.COM", "Netflix", d, false},
		{"chk", "mine", "-25.00", "GROCERY BY HAND", "", d, false}, // set by hand below
		// A card payment: out of checking, into the card two days later.
		{"chk", "pay-out", "-500.00", "PAYMENT TO CARD", "", d, false},
		{"card", "pay-in", "500.00", "PAYMENT THANK YOU", "", d.AddDate(0, 0, 2), false},
		// Ambiguous: one outflow, two matching inflows.
		{"chk", "amb-out", "-50.00", "TRANSFER", "", d, false},
		{"card", "amb-in1", "50.00", "TRANSFER", "", d, false},
		{"sav", "amb-in2", "50.00", "TRANSFER", "", d.AddDate(0, 0, 1), false},
		// Too far apart, and pending: not transfers.
		{"chk", "far-out", "-70.00", "MOVE", "", d, false},
		{"sav", "far-in", "70.00", "MOVE", "", d.AddDate(0, 0, 10), false},
		{"chk", "pend-out", "-80.00", "MOVE", "", d, true},
		{"sav", "pend-in", "80.00", "MOVE", "", d, false},
	} {
		posted := &x.at
		if x.pending {
			posted = nil
		}
		exec(`INSERT INTO transactions (account_id, id, posted_at, transacted_at, amount, description, payee, pending)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, x.acct, x.id, posted, x.at, x.amount, x.desc, x.payee, x.pending)
	}
	exec(`UPDATE transactions SET category_id = (SELECT id FROM categories WHERE name = 'Dining'),
		category_source = 'manual' WHERE id = 'mine'`)
	exec(`INSERT INTO rules (pattern, field, category_id, priority) VALUES
		('grocery', 'description', (SELECT id FROM categories WHERE name = 'Groceries'), 100),
		('netflix', 'payee', (SELECT id FROM categories WHERE name = 'Entertainment'), 50)`)

	type state struct {
		category string
		source   string
		transfer bool
	}
	get := func(id string) state {
		t.Helper()
		var s state
		if err := owner.QueryRow(ctx, `SELECT coalesce(c.name, ''), coalesce(t.category_source, ''), t.is_transfer
			FROM transactions t LEFT JOIN categories c ON c.id = t.category_id WHERE t.id = $1`, id).
			Scan(&s.category, &s.source, &s.transfer); err != nil {
			t.Fatal(err)
		}
		return s
	}
	check := func(want map[string]state) {
		t.Helper()
		for id, w := range want {
			if got := get(id); got != w {
				t.Errorf("%s: got %+v, want %+v", id, got, w)
			}
		}
	}

	c, err := st.Categorize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c != (Categorized{Ruled: 3, Cleared: 0, Transfers: 2}) {
		t.Errorf("first run: %+v", c)
	}
	check(map[string]state{
		"groc":     {"Groceries", "rule", false},
		"both":     {"Entertainment", "rule", false}, // priority 50 beats 100
		"flix":     {"Entertainment", "rule", false},
		"mine":     {"Dining", "manual", false}, // rules never override you
		"pay-out":  {"Transfer", "auto", true},
		"pay-in":   {"Transfer", "auto", true},
		"amb-out":  {"", "", false},
		"amb-in1":  {"", "", false},
		"far-out":  {"", "", false},
		"pend-out": {"", "", false},
		"pend-in":  {"", "", false},
	})

	// Running again changes nothing.
	if c, err := st.Categorize(ctx); err != nil || c != (Categorized{}) {
		t.Errorf("second run: %+v %v", c, err)
	}

	// Deleting a rule clears what it set; hand-set and detected stay.
	exec(`DELETE FROM rules WHERE pattern = 'grocery'`)
	if c, err := st.Categorize(ctx); err != nil || c != (Categorized{Cleared: 1}) {
		t.Errorf("after delete: %+v %v", c, err)
	}
	check(map[string]state{
		"groc":   {"", "", false},
		"mine":   {"Dining", "manual", false},
		"pay-in": {"Transfer", "auto", true},
	})

	// The sync role can't write categories directly.
	_, err = syncPool.Exec(ctx, `UPDATE transactions SET category_id = NULL WHERE id = 'mine'`)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("finance_sync updated category_id directly: %v", err)
	}
}
