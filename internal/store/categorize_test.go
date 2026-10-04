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
// write category columns or the history itself.
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
	for _, s := range []string{`DELETE FROM balance_snapshots`, `DELETE FROM categorizations`,
		`DELETE FROM transactions`, `DELETE FROM accounts`, `DELETE FROM connections`} {
		exec(s)
	}
	exec(`INSERT INTO accounts (id, name) VALUES ('chk', 'Checking'), ('card', 'Card'), ('sav', 'Savings')`)

	d := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	type txn struct {
		acct, id, amount, payee, desc string
		pending                       bool
	}
	for _, x := range []txn{
		// Categorized by hand (below).
		{"card", "sb1", "-5.00", "Starbucks", "STARBUCKS STORE 123", false},
		{"card", "amz-old1", "-20.00", "Amazon", "AMZN MKTP", false},
		{"card", "amz-old2", "-21.00", "Amazon", "AMZN MKTP", false},
		{"card", "amz-old3", "-22.00", "Amazon", "AMZN MKTP", false},
		{"card", "amz-new", "-30.00", "Amazon", "AMZN MKTP", false},
		{"card", "tgt1", "-40.00", "Target", "TARGET 0042", false},
		{"card", "tgt2", "-41.00", "Target", "TARGET 0042", false},
		{"chk", "pay1", "2000.00", "Acme Payroll", "ACME PAYROLL", false},
		// Not categorized yet.
		{"card", "sb2", "-6.10", "Starbucks", "STARBUCKS STORE 456", false},
		{"card", "sb-pend", "-4.00", "Starbucks", "STARBUCKS", true},
		{"card", "sb-refund", "5.00", "Starbucks", "STARBUCKS REFUND", false}, // money in: no money-in Starbucks choices
		{"card", "amz2", "-15.00", "Amazon", "AMZN MKTP", false},
		{"card", "tgt3", "-42.00", "Target", "TARGET 0042", false}, // your choices disagree: no guess
		{"card", "zoo", "-60.00", "Zebra Zoo Tickets", "", false},  // nothing similar
		{"chk", "pay2", "2000.00", "Acme Payroll", "ACME PAYROLL", false},
		// A card payment: out of checking, into the card.
		{"chk", "pay-out", "-500.00", "", "PAYMENT TO CARD", false},
		{"card", "pay-in", "500.00", "", "PAYMENT THANK YOU", false},
	} {
		var posted *time.Time
		if !x.pending {
			posted = &d
		}
		exec(`INSERT INTO transactions (account_id, id, posted_at, transacted_at, amount, payee, description, pending)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, x.acct, x.id, posted, d, x.amount, x.payee, x.desc, x.pending)
	}
	label := func(id, category string) {
		t.Helper()
		exec(`UPDATE transactions SET category_source = 'manual',
			category_id = (SELECT id FROM categories WHERE name = $2) WHERE id = $1`, id, category)
	}
	label("sb1", "Dining")
	for _, id := range []string{"amz-old1", "amz-old2", "amz-old3"} {
		label(id, "Shopping")
	}
	label("amz-new", "Kids")
	label("tgt1", "Shopping")
	label("tgt2", "Groceries")
	label("pay1", "Income")
	// Three Amazon choices from two years ago; the Kids one is today's.
	exec(`UPDATE categorizations SET categorized_at = now() - interval '730 days' WHERE transaction_id LIKE 'amz-old%'`)

	type state struct{ category, source string }
	get := func(id string) state {
		t.Helper()
		var s state
		if err := owner.QueryRow(ctx, `SELECT coalesce(c.name, ''), coalesce(t.category_source, '')
			FROM transactions t LEFT JOIN categories c ON c.id = t.category_id WHERE t.id = $1`, id).
			Scan(&s.category, &s.source); err != nil {
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
	if c != (Categorized{Learned: 4, Transfers: 2}) {
		t.Errorf("first run: %+v", c)
	}
	check(map[string]state{
		"sb2":       {"Dining", "learned"},
		"sb-pend":   {"Dining", "learned"},
		"sb-refund": {"", ""},
		"amz2":      {"Kids", "learned"}, // today's choice outweighs three old ones
		"tgt3":      {"", ""},
		"zoo":       {"", ""},
		"pay2":      {"Income", "learned"},
		"pay-out":   {"Transfer", "auto"},
		"pay-in":    {"Transfer", "auto"},
		"amz-new":   {"Kids", "manual"}, // choices are never changed
	})

	// After a sync, only uncategorized transactions are looked at again.
	if c, err := st.Categorize(ctx); err != nil || c != (Categorized{}) {
		t.Errorf("second run: %+v %v", c, err)
	}

	// Undo the Kids choice: Amazon guesses fall back on the older choices.
	// (recency shifts the balance between categories; it doesn't expire them).
	exec(`UPDATE transactions SET category_source = NULL, category_id = NULL WHERE id = 'amz-new'`)
	var learned int
	if err := syncPool.QueryRow(ctx, `SELECT learned FROM categorize('amazon')`).Scan(&learned); err != nil {
		t.Fatal(err)
	}
	if learned != 2 {
		t.Errorf("re-guessed %d Amazon transactions, want 2", learned)
	}
	check(map[string]state{"amz2": {"Shopping", "learned"}, "amz-new": {"Shopping", "learned"}})

	// Correcting a guess is recorded, with what the guess was.
	label("sb2", "Groceries")
	var category, prevCategory, prevSource string
	if err := owner.QueryRow(ctx, `SELECT coalesce(c.name, ''), coalesce(p.name, ''), coalesce(h.previous_source, '')
		FROM categorizations h
		LEFT JOIN categories c ON c.id = h.category_id
		LEFT JOIN categories p ON p.id = h.previous_category_id
		WHERE h.transaction_id = 'sb2' ORDER BY h.id DESC LIMIT 1`).Scan(&category, &prevCategory, &prevSource); err != nil {
		t.Fatal(err)
	}
	if category != "Groceries" || prevCategory != "Dining" || prevSource != "learned" {
		t.Errorf("history row: %q (was %q, %q)", category, prevCategory, prevSource)
	}
	var n int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM categorizations WHERE transaction_id = 'amz-new'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("amz-new history rows = %d (%v), want 2 (Kids, then back to automatic)", n, err)
	}

	// The sync role can't write categories or history directly.
	for _, sql := range []string{
		`UPDATE transactions SET category_id = NULL WHERE id = 'sb1'`,
		`INSERT INTO categorizations (account_id, transaction_id, source) VALUES ('card', 'sb1', 'manual')`,
	} {
		if _, err := syncPool.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("finance_sync: %s: %v", sql, err)
		}
	}
}
