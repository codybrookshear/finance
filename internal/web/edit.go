package web

// Edits: setting a transaction's category and note. Every write runs in a
// transaction as finance_edit (SET LOCAL ROLE; finance_web itself can't write
// and the role reverts when the transaction ends). The database records each
// category you set in categorizations, and categorize() then re-guesses the
// transactions similar to the one you changed.

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

// SaveTxn sets a transaction's category (nil = automatic: the app guesses)
// and note. It returns how many similar transactions got a new guess.
func (db *DB) SaveTxn(ctx context.Context, accountID, id string, categoryID *int32, note string) (guessed int, err error) {
	err = db.edit(ctx, func(tx pgx.Tx) error {
		var merchant string
		err := tx.QueryRow(ctx, `
			UPDATE transactions SET
				category_id = $3,
				is_transfer = coalesce((SELECT kind = 'transfer' FROM categories WHERE id = $3), false),
				category_source = CASE WHEN $3::integer IS NULL THEN NULL ELSE 'manual' END,
				note = nullif($4, '')
			WHERE account_id = $1 AND id = $2
			  AND account_id IN (SELECT id FROM accounts WHERE NOT hidden)
			RETURNING merchant`,
			accountID, id, categoryID, note).Scan(&merchant)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if merchant == "" {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT learned FROM categorize($1)`, merchant).Scan(&guessed)
	})
	return guessed, err
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

type editForm struct {
	Txn        Txn
	Categories []Category
	Selected   int32 // the hand-set category, 0 when automatic
}

func (s *Server) editForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t, err := s.cfg.DB.Txn(r.Context(), q.Get("account"), q.Get("id"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	cats, err := s.cfg.DB.Categories(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := editForm{Txn: t, Categories: cats}
	if t.CategoryID != nil && (t.Source == "manual" || t.Source == "claude") {
		f.Selected = *t.CategoryID
	}
	s.render(w, r, "transactions", "edit", f)
}

func (s *Server) saveTxn(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	account, id := r.PostForm.Get("account"), r.PostForm.Get("id")
	category, ok := parseID(r.PostForm.Get("category"))
	if !ok {
		badRequest(w, "Bad category")
		return
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if len(note) > 500 {
		badRequest(w, "Note is too long (500 characters at most)")
		return
	}
	guessed, err := s.cfg.DB.SaveTxn(r.Context(), account, id, category, note)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if isForeignKeyViolation(err) {
		badRequest(w, "No such category")
		return
	} else if err != nil {
		s.fail(w, r, fmt.Errorf("save transaction: %w", err))
		return
	}
	t, err := s.cfg.DB.Txn(r.Context(), account, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t.Open = true
	switch {
	case guessed == 1:
		t.Message = "Saved. Updated the guess for 1 similar transaction."
	case guessed > 1:
		t.Message = fmt.Sprintf("Saved. Updated the guess for %s similar transactions.", commas(guessed))
	default:
		t.Message = "Saved."
	}
	s.render(w, r, "transactions", "txn", t)
}
