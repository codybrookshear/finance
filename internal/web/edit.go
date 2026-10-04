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
			  AND account_id IN (SELECT id FROM accounts WHERE NOT hidden)
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
			  AND account_id IN (SELECT id FROM accounts WHERE NOT hidden)`,
			accountID, id, note)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrNotFound
		}
		return err
	})
}

// SetAccount saves your settings for an account: a name ("" = SimpleFIN's),
// whether it counts in net worth, and whether it's hidden everywhere.
func (db *DB) SetAccount(ctx context.Context, id, name string, inNetWorth, hidden bool) error {
	return db.edit(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE accounts SET display_name = nullif($2, ''), include_in_net_worth = $3, hidden = $4
			WHERE id = $1`, id, name, inNetWorth, hidden)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrNotFound
		}
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
	p := rowsPage{Rows: []txnRow{{Txn: t}}}
	others, err := s.cfg.DB.TxnsByKey(r.Context(), changed)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, o := range others {
		o.OOB = true
		p.Rows = append(p.Rows, txnRow{Txn: o})
	}
	if p.Categories, err = s.cfg.DB.Categories(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "transactions", "rows", p)
}

// setAccount saves an account's settings (sent whole on any change) and has
// htmx reload the page: totals, groups and duplicate flags all depend on it.
func (s *Server) setAccount(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if len(name) > 100 {
		badRequest(w, "Name is too long (100 characters at most)")
		return
	}
	err := s.cfg.DB.SetAccount(r.Context(), r.PostForm.Get("id"), name,
		r.PostForm.Get("networth") == "on", r.PostForm.Get("hidden") == "on")
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, r, fmt.Errorf("set account: %w", err))
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
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
	t.Open = true
	p := rowsPage{Rows: []txnRow{{Txn: t}}}
	if p.Categories, err = s.cfg.DB.Categories(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "transactions", "rows", p)
}
