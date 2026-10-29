package web

// Edits: a transaction's category (picked from its chip) and note, each saved
// as soon as it changes. Every write runs in a transaction as finance_edit
// (SET LOCAL ROLE; finance_web itself can't write and the role reverts when
// the transaction ends). The database records each category you set in
// categorizations, and categorize() then re-guesses similar transactions.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (db *DB) edit(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE finance_edit`); err != nil {
			return err
		}
		return fn(tx)
	})
}

// TxnKey identifies a transaction.
type TxnKey struct{ AccountID, ID string }

// SetCategory sets a transaction's category by hand (nil = back to automatic:
// the app guesses) and re-guesses similar transactions. It returns the most
// recent other transactions whose category changed as a result (at most 100:
// enough to refresh what's on screen).
func (db *DB) SetCategory(ctx context.Context, accountID, id string, categoryID *int32) (changed []TxnKey, err error) {
	err = db.edit(ctx, func(tx pgx.Tx) error {
		var merchant string
		err := tx.QueryRow(ctx, `
			UPDATE transactions SET
				category_id = $3,
				is_transfer = coalesce((SELECT kind = 'transfer' FROM categories WHERE id = $3), false),
				category_source = CASE WHEN $3::integer IS NULL THEN NULL ELSE 'manual' END
			WHERE account_id = $1 AND id = $2
			  AND account_id IN (SELECT a.id FROM accounts a WHERE `+shownAccount+`)
			RETURNING merchant`,
			accountID, id, categoryID).Scan(&merchant)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil || merchant == "" {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT categorize($1)`, merchant); err != nil {
			return err
		}
		// Rows this transaction wrote carry its transaction ID as xmin.
		rows, err := tx.Query(ctx, `
			SELECT account_id, id FROM transactions
			WHERE xmin = pg_current_xact_id()::xid AND NOT (account_id = $1 AND id = $2)
			ORDER BY coalesce(transacted_at, posted_at, first_seen_at) DESC
			LIMIT 100`, accountID, id)
		if err != nil {
			return err
		}
		changed, err = pgx.CollectRows(rows, pgx.RowToStructByPos[TxnKey])
		return err
	})
	return changed, err
}

// SetNote sets a transaction's note ("" removes it).
func (db *DB) SetNote(ctx context.Context, accountID, id, note string) error {
	return db.edit(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE transactions SET note = nullif($3, '')
			WHERE account_id = $1 AND id = $2
			  AND account_id IN (SELECT a.id FROM accounts a WHERE `+shownAccount+`)`,
			accountID, id, note)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrNotFound
		}
		return err
	})
}

// ExpectedIncome is the monthly income set by hand (nil if not set).
func (db *DB) ExpectedIncome(ctx context.Context) (*string, error) {
	var amount *string
	err := db.Pool.QueryRow(ctx, `SELECT round(amount, 2)::text FROM expected_income`).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return amount, err
}

// SetExpectedIncome sets the expected monthly income (nil clears it).
func (db *DB) SetExpectedIncome(ctx context.Context, amount *string) error {
	return db.edit(ctx, func(tx pgx.Tx) error {
		if amount == nil {
			_, err := tx.Exec(ctx, `DELETE FROM expected_income`)
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO expected_income (id, amount) VALUES (1, $1::numeric)
			ON CONFLICT (id) DO UPDATE SET amount = EXCLUDED.amount`, *amount)
		return err
	})
}

// Targets are the monthly spending targets set by hand, by category ID.
func (db *DB) Targets(ctx context.Context) (map[int32]string, error) {
	rows, err := db.Pool.Query(ctx, `SELECT category_id, round(amount, 2)::text FROM category_targets`)
	if err != nil {
		return nil, err
	}
	targets := map[int32]string{}
	var id int32
	var amount string
	_, err = pgx.ForEachRow(rows, []any{&id, &amount}, func() error {
		targets[id] = amount
		return nil
	})
	return targets, err
}

// SetTarget sets an expense category's monthly spending target (nil clears
// it). ErrNotFound if there's no such expense category.
func (db *DB) SetTarget(ctx context.Context, categoryID int32, amount *string) error {
	return db.edit(ctx, func(tx pgx.Tx) error {
		var ok bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM categories WHERE id = $1 AND kind = 'expense')`, categoryID).Scan(&ok)
		if err != nil {
			return err
		} else if !ok {
			return ErrNotFound
		}
		if amount == nil {
			_, err = tx.Exec(ctx, `DELETE FROM category_targets WHERE category_id = $1`, categoryID)
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO category_targets (category_id, amount) VALUES ($1, $2::numeric)
			ON CONFLICT (category_id) DO UPDATE SET amount = EXCLUDED.amount`, categoryID, *amount)
		return err
	})
}

// ---- handlers ----

const maxFormBytes = 16 << 10

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func (s *Server) parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

// parseID reads "" (none) or a positive database ID.
func parseID(v string) (*int32, bool) {
	if v == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
		return nil, false
	}
	id := int32(n)
	return &id, true
}

// setCategory saves a category picked from a row's chip and responds with
// that row, plus out-of-band updates for the rows whose guess changed.
func (s *Server) setCategory(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	account, id := r.PostForm.Get("account"), r.PostForm.Get("id")
	category, ok := parseID(r.PostForm.Get("category"))
	if !ok {
		badRequest(w, "Bad category")
		return
	}
	changed, err := s.cfg.DB.SetCategory(r.Context(), account, id, category)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if isForeignKeyViolation(err) {
		badRequest(w, "No such category")
		return
	} else if err != nil {
		s.fail(w, r, fmt.Errorf("set category: %w", err))
		return
	}
	t, err := s.cfg.DB.Txn(r.Context(), account, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t.Dated = fromSpending(r)
	p := rowsPage{Rows: []txnRow{{Txn: t}}}
	others, err := s.cfg.DB.TxnsByKey(r.Context(), changed)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, o := range others {
		o.OOB, o.Dated = true, t.Dated
		p.Rows = append(p.Rows, txnRow{Txn: o})
	}
	if p.Categories, err = s.cfg.DB.Categories(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "transactions", "rows", p)
}

func (s *Server) setNote(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if len(note) > 500 {
		badRequest(w, "Note is too long (500 characters at most)")
		return
	}
	account, id := r.PostForm.Get("account"), r.PostForm.Get("id")
	err := s.cfg.DB.SetNote(r.Context(), account, id, note)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, r, fmt.Errorf("set note: %w", err))
		return
	}
	// The note is the row's title now: send the row back, details still open.
	t, err := s.cfg.DB.Txn(r.Context(), account, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t.Open, t.Dated = true, fromSpending(r)
	p := rowsPage{Rows: []txnRow{{Txn: t}}}
	if p.Categories, err = s.cfg.DB.Categories(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "transactions", "rows", p)
}

// formAmount reads the form's amount, typed like "$15,000.50" (nil when
// blank or reset). It answers 400 and returns false if it can't be read.
func formAmount(w http.ResponseWriter, r *http.Request) (*string, bool) {
	v := strings.ReplaceAll(strings.TrimSpace(r.PostForm.Get("amount")), ",", "")
	if v == "" || r.PostForm.Get("reset") != "" {
		return nil, true
	}
	m := amountRE.FindStringSubmatch(v)
	if m == nil {
		badRequest(w, "Enter an amount like 15000 or 15000.50")
		return nil, false
	}
	return &m[1], true
}

// backToSpending returns to the Spending page's month the form came from.
func backToSpending(w http.ResponseWriter, r *http.Request) {
	to := "/spending"
	if m := r.PostForm.Get("month"); monthRE.MatchString(m) {
		to += "?month=" + m
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// setExpectedIncome saves the expected monthly income typed on the Spending
// page ("" puts it back to last month's income), then returns to that month.
func (s *Server) setExpectedIncome(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	amount, ok := formAmount(w, r)
	if !ok {
		return
	}
	if err := s.cfg.DB.SetExpectedIncome(r.Context(), amount); err != nil {
		s.fail(w, r, fmt.Errorf("set expected income: %w", err))
		return
	}
	backToSpending(w, r)
}

// setTarget saves an expense category's monthly spending target typed on the
// Spending page ("" removes it), then returns to that month.
func (s *Server) setTarget(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PostForm.Get("category"), 10, 32)
	if err != nil {
		badRequest(w, "Pick a category")
		return
	}
	amount, ok := formAmount(w, r)
	if !ok {
		return
	}
	err = s.cfg.DB.SetTarget(r.Context(), int32(id), amount)
	if errors.Is(err, ErrNotFound) {
		badRequest(w, "No such spending category")
		return
	} else if err != nil {
		s.fail(w, r, fmt.Errorf("set target: %w", err))
		return
	}
	backToSpending(w, r)
}
