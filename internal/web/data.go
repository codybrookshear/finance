package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Read-only queries for the UI. They run as finance_web, which can only
// SELECT. Amounts come back as round(x, 2)::text and stay strings.

// Account is a visible account: not hidden, and not a duplicate copy of
// another (see store.MarkDuplicateAccounts).
type Account struct {
	ID, Name, Org, Currency, Balance string
	BalanceAt                        *time.Time
	InNetWorth                       bool
	OrgTotal                         string // the institution's accounts counted in net worth
	AlsoUnder                        string // institutions whose duplicate copies were folded into this one
}

// shownAccount is the SQL test for accounts the app shows (alias a).
const shownAccount = `NOT a.hidden AND a.duplicate_of IS NULL`

type Txn struct {
	AccountID, ID, Account, Currency string
	At                               time.Time
	Amount, Description, Payee, Memo string
	Category                         string // name, "" if none
	CategoryID                       *int32
	CategoryEmoji                    string
	Source                           string // rule, auto, manual, claude, or "" (none)
	Note                             string
	Pending, Transfer                bool
	OOB                              bool // view only: render as an htmx out-of-band swap
	Open                             bool // view only: render with the details open
}

// DOMID identifies the transaction's row in the page.
func (t Txn) DOMID() string {
	return "t-" + base64.RawURLEncoding.EncodeToString([]byte(t.AccountID+"\x00"+t.ID))
}

// Guessed reports whether the category was filled in automatically.
func (t Txn) Guessed() bool { return t.Source == "learned" || t.Source == "auto" }

// Chosen is the category you set by hand (0 if none).
func (t Txn) Chosen() int32 {
	if t.CategoryID != nil && (t.Source == "manual" || t.Source == "claude") {
		return *t.CategoryID
	}
	return 0
}

// Merchant is SimpleFIN's name for the other party: the payee (a cleaned-up
// name, from SimpleFIN Bridge) if there is one, else the bank's description.
func (t Txn) Merchant() string {
	if p := strings.TrimSpace(t.Payee); p != "" {
		return p
	}
	return t.Description
}

// Title is what the list shows: your note if you wrote one, else the merchant.
func (t Txn) Title() string {
	if n := strings.TrimSpace(t.Note); n != "" {
		return n
	}
	return t.Merchant()
}

// Category is one of the categories a transaction can have.
type Category struct {
	ID    int32
	Name  string
	Kind  string // expense, income or transfer
	Emoji string
	Hue   *int32 // chip color; nil = neutral
}

func (db *DB) Categories(ctx context.Context) ([]Category, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, name, kind, emoji, hue FROM categories
		ORDER BY kind = 'transfer', kind = 'income', name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Category])
}

// TxnFilter selects transactions. Zero values mean "no filter".
type TxnFilter struct {
	Query     string
	AccountID string
	From, To  *time.Time // [From, To)
	Category  string     // "" all, "none" uncategorized, "learned" guessed, or a category ID
	After     *Cursor    // next page: rows strictly older than this
	Limit     int
}

// Cursor marks a position in the newest-first transaction list.
type Cursor struct {
	At            time.Time
	AccountID, ID string
}

func (c Cursor) String() string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.At.UTC().Format(time.RFC3339Nano) + "\x00" + c.AccountID + "\x00" + c.ID))
}

func parseCursor(s string) (*Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) > 1024 {
		return nil, errors.New("bad cursor")
	}
	p := strings.Split(string(b), "\x00")
	if len(p) != 3 {
		return nil, errors.New("bad cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, p[0])
	if err != nil {
		return nil, errors.New("bad cursor")
	}
	return &Cursor{At: at, AccountID: p[1], ID: p[2]}, nil
}

// Total is a sum of amounts in one currency.
type Total struct{ Currency, Amount string }

// Line is a labelled amount, e.g. one category's spending in a month.
type Line struct {
	Label, Amount string
	CategoryID    *int32 // nil for "Uncategorized"
	Emoji         string
	URL           string // the month's transactions in this category (set by the handler)
}

type DB struct{ Pool *pgxpool.Pool }

func (db *DB) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT a.id, coalesce(a.display_name, a.name), a.org_name, a.currency,
		       round(a.balance, 2)::text, a.balance_at, a.include_in_net_worth,
		       round(coalesce(sum(a.balance) FILTER (WHERE a.include_in_net_worth)
		                      OVER (PARTITION BY a.org_name), 0), 2)::text,
		       coalesce((SELECT string_agg(DISTINCT d.org_name, ', ') FROM accounts d
		                 WHERE d.duplicate_of = a.id AND d.org_name <> a.org_name), '')
		FROM accounts a WHERE `+shownAccount+`
		ORDER BY lower(a.org_name), a.include_in_net_worth DESC, lower(coalesce(a.display_name, a.name))`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) {
		var a Account
		err := r.Scan(&a.ID, &a.Name, &a.Org, &a.Currency, &a.Balance, &a.BalanceAt, &a.InNetWorth, &a.OrgTotal, &a.AlsoUnder)
		return a, err
	})
}

const (
	txnSortAt = `coalesce(t.transacted_at, t.posted_at, t.first_seen_at)`
	txnFrom   = `
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN categories c ON c.id = t.category_id`
	// $1 query, $2 LIKE pattern, $3 amount (or NULL), $4 account, $5 from, $6 to, $7 category
	txnWhere = `
		WHERE ` + shownAccount + `
		  AND ($1::text = ''
		       OR t.search @@ websearch_to_tsquery('simple', $1)
		       OR t.description ILIKE $2 OR t.payee ILIKE $2 OR t.memo ILIKE $2 OR t.note ILIKE $2
		       OR ($3::text IS NOT NULL AND abs(t.amount) = $3::text::numeric))
		  AND ($4::text = '' OR t.account_id = $4)
		  AND ($5::timestamptz IS NULL OR ` + txnSortAt + ` >= $5)
		  AND ($6::timestamptz IS NULL OR ` + txnSortAt + ` < $6)
		  AND ($7::text = ''
		       OR ($7 = 'none' AND t.category_id IS NULL AND NOT t.is_transfer)
		       OR ($7 = 'learned' AND t.category_source = 'learned')
		       OR t.category_id::text = $7)`
	txnCols = `t.account_id, t.id, coalesce(a.display_name, a.name), a.currency, ` + txnSortAt + `,
		round(t.amount, 2)::text, t.description, t.payee, t.memo,
		coalesce(c.name, ''), t.category_id, coalesce(c.emoji, ''), coalesce(t.category_source, ''), coalesce(t.note, ''),
		t.pending, t.is_transfer`
)

var amountRE = regexp.MustCompile(`^\$?(\d{1,9}(?:\.\d{1,2})?)$`)

func (f TxnFilter) args() []any {
	q := strings.TrimSpace(f.Query)
	var amount *string
	if m := amountRE.FindStringSubmatch(strings.ReplaceAll(q, ",", "")); m != nil {
		amount = &m[1]
	}
	return []any{q, "%" + escapeLike(q) + "%", amount, f.AccountID, f.From, f.To, f.Category}
}

// escapeLike makes user input match literally inside ILIKE '%...%'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Transactions returns one page, newest first, and the cursor for the next
// page (nil when there are no more).
func (db *DB) Transactions(ctx context.Context, f TxnFilter) ([]Txn, *Cursor, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	args := f.args()
	var cAt *time.Time
	var cAcct, cID string
	if f.After != nil {
		cAt, cAcct, cID = &f.After.At, f.After.AccountID, f.After.ID
	}
	args = append(args, cAt, cAcct, cID, f.Limit+1)
	rows, err := db.Pool.Query(ctx, `SELECT `+txnCols+txnFrom+txnWhere+`
		  AND ($8::timestamptz IS NULL OR (`+txnSortAt+`, t.account_id, t.id) < ($8, $9, $10))
		ORDER BY 5 DESC, t.account_id DESC, t.id DESC
		LIMIT $11`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("transactions: %w", err)
	}
	txns, err := pgx.CollectRows(rows, scanTxn)
	if err != nil {
		return nil, nil, fmt.Errorf("transactions: %w", err)
	}
	var next *Cursor
	if len(txns) > f.Limit {
		txns = txns[:f.Limit]
		last := txns[len(txns)-1]
		next = &Cursor{At: last.At, AccountID: last.AccountID, ID: last.ID}
	}
	return txns, next, nil
}

func scanTxn(r pgx.CollectableRow) (Txn, error) {
	var t Txn
	err := r.Scan(&t.AccountID, &t.ID, &t.Account, &t.Currency, &t.At, &t.Amount,
		&t.Description, &t.Payee, &t.Memo, &t.Category, &t.CategoryID, &t.CategoryEmoji, &t.Source, &t.Note,
		&t.Pending, &t.Transfer)
	return t, err
}

// Txn returns one transaction (ErrNotFound if it doesn't exist or its
// account is hidden).
func (db *DB) Txn(ctx context.Context, accountID, id string) (Txn, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+txnCols+txnFrom+`
		WHERE `+shownAccount+` AND t.account_id = $1 AND t.id = $2`, accountID, id)
	if err != nil {
		return Txn{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTxn)
	if errors.Is(err, pgx.ErrNoRows) {
		return Txn{}, ErrNotFound
	}
	return t, err
}

var ErrNotFound = errors.New("not found")

// TxnsByKey returns the given transactions (those in hidden accounts are
// left out), newest first.
func (db *DB) TxnsByKey(ctx context.Context, keys []TxnKey) ([]Txn, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	accounts, ids := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		accounts[i], ids[i] = k.AccountID, k.ID
	}
	rows, err := db.Pool.Query(ctx, `SELECT `+txnCols+txnFrom+`
		WHERE `+shownAccount+` AND (t.account_id, t.id) IN (SELECT * FROM unnest($1::text[], $2::text[]))
		ORDER BY 5 DESC`, accounts, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanTxn)
}

// TxnSummary counts and totals everything the filter matches (all pages).
func (db *DB) TxnSummary(ctx context.Context, f TxnFilter) (count int, totals []Total, err error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT a.currency, count(*), round(sum(t.amount), 2)::text`+txnFrom+txnWhere+`
		GROUP BY a.currency ORDER BY count(*) DESC`, f.args()...)
	if err != nil {
		return 0, nil, fmt.Errorf("summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t Total
		var n int
		if err := rows.Scan(&t.Currency, &n, &t.Amount); err != nil {
			return 0, nil, fmt.Errorf("summary: %w", err)
		}
		count += n
		totals = append(totals, t)
	}
	return count, totals, rows.Err()
}

// Month is one month of spending and income.
type Month struct {
	YearMonth        string // YYYY-MM
	Spent, Income    string
	Net              string // Income - Spent
	Categories       []Line // spending per category, largest first
	IncomeCategories []Line
}

// Spending summarizes posted, non-transfer transactions per month since
// `since`. Spending is shown positive: refunds in an expense category reduce
// it. Uncategorized money in counts as income, money out as spending. Net is
// income minus spending (the plain sum of the month's amounts).
func (db *DB) Spending(ctx context.Context, tz string, since time.Time) ([]Month, error) {
	rows, err := db.Pool.Query(ctx, `
		WITH tx AS (
			SELECT to_char(date_trunc('month', `+txnSortAt+` AT TIME ZONE $1), 'YYYY-MM') AS month,
			       CASE WHEN c.kind = 'income' OR (c.kind IS NULL AND t.amount > 0)
			            THEN 'income' ELSE 'expense' END AS kind,
			       coalesce(c.name, 'Uncategorized') AS category,
			       c.id AS category_id, coalesce(c.emoji, '') AS emoji,
			       t.amount`+txnFrom+`
			WHERE `+shownAccount+` AND NOT t.pending AND NOT t.is_transfer
			  AND coalesce(c.kind, '') <> 'transfer'
			  AND `+txnSortAt+` >= $2
		)
		SELECT month, kind, category, category_id, emoji,
		       round(CASE WHEN kind = 'expense' THEN -sum(amount) ELSE sum(amount) END, 2)::text
		FROM tx
		GROUP BY GROUPING SETS ((month, kind, category, category_id, emoji), (month, kind), (month))
		ORDER BY month DESC, kind NULLS FIRST, category IS NOT NULL,
		         CASE WHEN kind = 'expense' THEN -sum(amount) ELSE sum(amount) END DESC`, tz, since)
	if err != nil {
		return nil, fmt.Errorf("spending: %w", err)
	}
	defer rows.Close()
	var months []Month
	for rows.Next() {
		var month, total string
		var kind, category, emoji *string
		var categoryID *int32
		if err := rows.Scan(&month, &kind, &category, &categoryID, &emoji, &total); err != nil {
			return nil, fmt.Errorf("spending: %w", err)
		}
		if len(months) == 0 || months[len(months)-1].YearMonth != month {
			months = append(months, Month{YearMonth: month, Spent: "0.00", Income: "0.00", Net: "0.00"})
		}
		m := &months[len(months)-1]
		switch {
		case kind == nil:
			m.Net = total
		case category == nil && *kind == "expense":
			m.Spent = total
		case category == nil:
			m.Income = total
		case *kind == "expense":
			m.Categories = append(m.Categories, Line{Label: *category, Amount: total, CategoryID: categoryID, Emoji: deref(emoji)})
		default:
			m.IncomeCategories = append(m.IncomeCategories, Line{Label: *category, Amount: total, CategoryID: categoryID, Emoji: deref(emoji)})
		}
	}
	return months, rows.Err()
}

// Point is one day of net worth.
type Point struct {
	Day   time.Time
	Value string
}

// NetWorth is the daily sum of balance snapshots across accounts counted in
// net worth. Days without a snapshot carry the account's last known balance.
func (db *DB) NetWorth(ctx context.Context) ([]Point, error) {
	rows, err := db.Pool.Query(ctx, `
		WITH acct AS (SELECT a.id FROM accounts a WHERE a.include_in_net_worth AND `+shownAccount+`),
		days AS (
			SELECT generate_series(min(as_of), max(as_of), interval '1 day')::date AS d
			FROM balance_snapshots WHERE account_id IN (SELECT id FROM acct)
		)
		SELECT days.d, round(sum(s.balance), 2)::text
		FROM days CROSS JOIN acct
		JOIN LATERAL (
			SELECT balance FROM balance_snapshots b
			WHERE b.account_id = acct.id AND b.as_of <= days.d
			ORDER BY b.as_of DESC LIMIT 1
		) s ON true
		GROUP BY days.d ORDER BY days.d`)
	if err != nil {
		return nil, fmt.Errorf("net worth: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Point, error) {
		var p Point
		err := r.Scan(&p.Day, &p.Value)
		return p, err
	})
}

// NetWorthNow totals current balances of accounts counted in net worth.
func (db *DB) NetWorthNow(ctx context.Context) ([]Total, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT a.currency, round(sum(a.balance), 2)::text FROM accounts a
		WHERE a.include_in_net_worth AND `+shownAccount+`
		GROUP BY a.currency ORDER BY a.currency`)
	if err != nil {
		return nil, fmt.Errorf("net worth: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Total])
}

// LastSync is when the last successful sync finished (nil if never).
func (db *DB) LastSync(ctx context.Context) (*time.Time, error) {
	var at *time.Time
	err := db.Pool.QueryRow(ctx, `SELECT max(finished_at) FROM sync_runs WHERE status = 'ok'`).Scan(&at)
	return at, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
