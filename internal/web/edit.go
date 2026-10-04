package web

// Edits: setting a transaction's category or note, and managing rules. Every
// write runs in a transaction as finance_edit (SET LOCAL ROLE; finance_web
// itself can't write and the role reverts when the transaction ends), and
// finishes with categorize() so rules and transfer detection catch up.

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
		if err := fn(tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT categorize()`)
		return err
	})
}

// NewRule is "when Field contains Pattern, use CategoryID".
type NewRule struct {
	Field, Pattern string
	CategoryID     int32
}

// SaveTxn sets a transaction's category (nil = automatic: rules and transfer
// detection decide) and note, and optionally adds a rule.
func (db *DB) SaveTxn(ctx context.Context, accountID, id string, categoryID *int32, note string, rule *NewRule) error {
	return db.edit(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE transactions SET
				category_id = $3,
				is_transfer = coalesce((SELECT kind = 'transfer' FROM categories WHERE id = $3), false),
				category_source = CASE WHEN $3::integer IS NULL THEN NULL ELSE 'manual' END,
				note = nullif($4, '')
			WHERE account_id = $1 AND id = $2
			  AND account_id IN (SELECT id FROM accounts WHERE NOT hidden)`,
			accountID, id, categoryID, note)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		if rule != nil {
			return addRule(ctx, tx, *rule)
		}
		return nil
	})
}

func addRule(ctx context.Context, tx pgx.Tx, r NewRule) error {
	// Same pattern and field again replaces the old rule's category.
	_, err := tx.Exec(ctx, `
		INSERT INTO rules (field, pattern, category_id, is_transfer)
		VALUES ($1, $2, $3, (SELECT kind = 'transfer' FROM categories WHERE id = $3))
		ON CONFLICT (field, lower(pattern)) DO UPDATE
		SET category_id = excluded.category_id, is_transfer = excluded.is_transfer`,
		r.Field, r.Pattern, r.CategoryID)
	return err
}

func (db *DB) AddRule(ctx context.Context, r NewRule) error {
	return db.edit(ctx, func(tx pgx.Tx) error { return addRule(ctx, tx, r) })
}

func (db *DB) DeleteRule(ctx context.Context, id int32) error {
	return db.edit(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM rules WHERE id = $1`, id)
		return err
	})
}

// Rule is a rule as listed on the rules page.
type Rule struct {
	ID             int32
	Field, Pattern string
	Category       string
	Transfer       bool
	Matches        int // transactions whose field contains the pattern
}

func (db *DB) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT r.id, r.field, r.pattern, coalesce(c.name, ''), r.is_transfer,
		       (SELECT count(*) FROM transactions t
		        WHERE strpos(lower(CASE r.field WHEN 'payee' THEN t.payee WHEN 'memo' THEN t.memo
		                                        ELSE t.description END), lower(r.pattern)) > 0)
		FROM rules r LEFT JOIN categories c ON c.id = r.category_id
		ORDER BY r.priority, lower(r.pattern)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Rule])
}

// ---- handlers ----

const maxFormBytes = 16 << 10

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

var ruleFields = map[string]bool{"description": true, "payee": true, "memo": true}

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
	Selected   int32  // the hand-set category, 0 when automatic
	RuleField  string // the field a new rule would match
	RuleText   string // suggested pattern
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
	f := editForm{Txn: t, Categories: cats, RuleField: "payee", RuleText: strings.TrimSpace(t.Payee)}
	if t.CategoryID != nil && (t.Source == "manual" || t.Source == "claude") {
		f.Selected = *t.CategoryID
	}
	if f.RuleText == "" {
		f.RuleField, f.RuleText = "description", strings.TrimSpace(t.Description)
	}
	f.RuleText = clip(f.RuleText, 200)
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
	var rule *NewRule
	if r.PostForm.Get("rule") == "on" && category != nil {
		rule = &NewRule{
			Field:      r.PostForm.Get("rule_field"),
			Pattern:    strings.TrimSpace(r.PostForm.Get("rule_pattern")),
			CategoryID: *category,
		}
		if !ruleFields[rule.Field] || rule.Pattern == "" || len(rule.Pattern) > 200 {
			badRequest(w, "A rule needs text to match (200 characters at most)")
			return
		}
	}
	err := s.cfg.DB.SaveTxn(r.Context(), account, id, category, note, rule)
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
	if rule != nil { // other rows on the page may have changed too
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	t, err := s.cfg.DB.Txn(r.Context(), account, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t.Open = true
	s.render(w, r, "transactions", "txn", t)
}

func (s *Server) rules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := struct {
		base
		Rules      []Rule
		Categories []Category
	}{}
	var err error
	if p.Rules, err = s.cfg.DB.Rules(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if p.Categories, err = s.cfg.DB.Categories(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if p.base, err = s.base(r, "rules"); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "rules", "layout.html", p)
}

func (s *Server) addRule(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	category, ok := parseID(r.PostForm.Get("category"))
	rule := NewRule{Field: r.PostForm.Get("field"), Pattern: strings.TrimSpace(r.PostForm.Get("pattern"))}
	if !ok || category == nil || !ruleFields[rule.Field] || rule.Pattern == "" || len(rule.Pattern) > 200 {
		badRequest(w, "A rule needs a field, text to match (200 characters at most) and a category")
		return
	}
	rule.CategoryID = *category
	if err := s.cfg.DB.AddRule(r.Context(), rule); isForeignKeyViolation(err) {
		badRequest(w, "No such category")
		return
	} else if err != nil {
		s.fail(w, r, fmt.Errorf("add rule: %w", err))
		return
	}
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	id, ok := parseID(r.PostForm.Get("id"))
	if !ok || id == nil {
		badRequest(w, "Bad rule")
		return
	}
	if err := s.cfg.DB.DeleteRule(r.Context(), *id); err != nil {
		s.fail(w, r, fmt.Errorf("delete rule: %w", err))
		return
	}
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}
