// Package syncer pulls data from SimpleFIN into Postgres.
//
// Each run does:
//  1. An incremental fetch (with pending transactions) from the last
//     successful sync minus an overlap, which also records today's balances.
//  2. If history is not yet complete, a few backfill fetches walking
//     backwards in windows of < 90 days (SimpleFIN Bridge's limit).
//
// The request budget per run keeps the job well under SimpleFIN Bridge's
// guidance of ~24 requests per day.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"finance/internal/simplefin"
	"finance/internal/store"
)

// Fetcher is the subset of the SimpleFIN client the syncer needs.
type Fetcher interface {
	Accounts(ctx context.Context, q simplefin.Query) (*simplefin.AccountSet, error)
}

type Config struct {
	Location           *time.Location // for daily balance snapshots
	Window             time.Duration  // per-request date range; must be < 90 days
	Overlap            time.Duration  // re-fetch this much before last success
	MaxBackfillPerRun  int            // backfill requests per run
	MaxLookback        time.Duration  // stop backfilling past this age
	EmptyWindowsToStop int            // consecutive empty windows that end backfill
	Now                func() time.Time
}

func DefaultConfig() Config {
	return Config{
		Location:           time.UTC,
		Window:             45 * 24 * time.Hour, // SimpleFIN's recommended maximum range
		Overlap:            5 * 24 * time.Hour,
		MaxBackfillPerRun:  2, // with 8 runs/day: ≤24 requests/day while backfilling
		MaxLookback:        3 * 365 * 24 * time.Hour,
		EmptyWindowsToStop: 2,
		Now:                time.Now,
	}
}

type Syncer struct {
	Store  *store.Store
	Client Fetcher
	Cfg    Config
	Log    *slog.Logger
}

type Result struct {
	Requests     int
	NewAccounts  int // accounts seen for the first time
	TxnsSeen     int
	StaleDeleted int
	Snapshots    int
	Messages     []string
}

// Run performs one sync. It records the run in sync_runs either way.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	runID, err := s.Store.StartRun(ctx)
	if err != nil {
		return res, fmt.Errorf("start run: %w", err)
	}
	err = s.run(ctx, &res)
	msg := strings.Join(res.Messages, "; ")
	if err != nil {
		msg = strings.TrimPrefix(msg+"; "+err.Error(), "; ")
	}
	if ferr := s.Store.FinishRun(context.WithoutCancel(ctx), runID, err == nil, res.Requests, res.TxnsSeen, msg); ferr != nil && err == nil {
		err = fmt.Errorf("finish run: %w", ferr)
	}
	return res, err
}

func (s *Syncer) run(ctx context.Context, res *Result) error {
	now := s.Cfg.Now().UTC()
	st, err := s.Store.State(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// --- 1. Incremental (includes pending + balances) ---
	start := now.Add(-s.Cfg.Window)
	if st.LastSuccessAt != nil {
		if s2 := st.LastSuccessAt.Add(-s.Cfg.Overlap); s2.After(start) {
			start = s2
		}
	}
	// After a long outage, cover the gap with older windows first.
	// Those go through backfill-style fetches (no pending, no balances).
	if st.LastSuccessAt != nil {
		gapStart := st.LastSuccessAt.Add(-s.Cfg.Overlap)
		for end := start; end.After(gapStart); end = end.Add(-s.Cfg.Window) {
			ws := maxTime(end.Add(-s.Cfg.Window), gapStart)
			if _, err := s.fetchAndStore(ctx, res, ws, end, false); err != nil {
				return err
			}
		}
	}
	if _, err := s.fetchAndStore(ctx, res, start, now, true); err != nil {
		return err
	}
	st.LastSuccessAt = &now
	if res.NewAccounts > 0 && st.BackfillBefore != nil {
		// A bank connected since the last run only got the window above: walk
		// back again so its history comes in too. Re-fetching the other
		// accounts' history is harmless (upserts; user fields untouched).
		st.BackfillDone, st.BackfillEmptyRuns, st.BackfillBefore = false, 0, &start
		s.Log.Info("new account: restarting backfill", "new_accounts", res.NewAccounts)
	}
	if st.BackfillBefore == nil {
		st.BackfillBefore = &start
	}
	if err := s.Store.SaveState(ctx, st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// --- 2. Backfill ---
	oldest := now.Add(-s.Cfg.MaxLookback)
	for i := 0; i < s.Cfg.MaxBackfillPerRun && !st.BackfillDone; i++ {
		end := *st.BackfillBefore
		if !end.After(oldest) {
			st.BackfillDone = true
			break
		}
		ws := maxTime(end.Add(-s.Cfg.Window), oldest)
		n, err := s.fetchAndStore(ctx, res, ws, end, false)
		if err != nil {
			return err
		}
		st.BackfillBefore = &ws
		if n == 0 {
			st.BackfillEmptyRuns++
		} else {
			st.BackfillEmptyRuns = 0
		}
		if st.BackfillEmptyRuns >= s.Cfg.EmptyWindowsToStop {
			st.BackfillDone = true
		}
		if err := s.Store.SaveState(ctx, st); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}
	if st.BackfillDone {
		s.Log.Info("backfill complete", "oldest_window_start", st.BackfillBefore)
	}
	return nil
}

// fetchAndStore fetches [start, end) and writes it. With current=true it
// includes pending transactions, reconciles stale pending rows and records
// balances; otherwise it only adds history. Returns the number of
// transactions the server returned.
func (s *Syncer) fetchAndStore(ctx context.Context, res *Result, start, end time.Time, current bool) (int, error) {
	res.Requests++
	set, err := s.Client.Accounts(ctx, simplefin.Query{Start: start, End: end, Pending: current})
	if err != nil {
		if errors.Is(err, simplefin.ErrAuth) {
			return 0, fmt.Errorf("%w — generate a new setup token in SimpleFIN Bridge and re-claim", err)
		}
		return 0, err
	}
	for _, m := range set.Messages() {
		// Server messages are about connection health ("re-authenticate
		// with your bank"); they contain no credentials.
		s.Log.Warn("simplefin message", "msg", m)
		res.Messages = appendUnique(res.Messages, m)
	}

	connNames := map[string]simplefin.Connection{}
	for _, c := range set.Connections {
		connNames[c.ConnID] = c
	}

	returned := 0
	err = s.Store.Tx(ctx, func(tx pgx.Tx) error {
		for _, a := range set.Accounts {
			connID, orgName := a.ConnID, ""
			if c, ok := connNames[connID]; ok {
				orgName = c.Name
				if err := store.UpsertConnection(ctx, tx, store.Connection{ID: c.ConnID, Name: c.Name, OrgURL: c.OrgURL}); err != nil {
					return err
				}
			} else if a.Org != nil { // protocol v1
				orgName = a.Org.Name
				if orgName == "" {
					orgName = a.Org.Domain
				}
				connID = "v1:" + firstNonEmpty(a.Org.ID, a.Org.Domain, a.Org.Name)
				if err := store.UpsertConnection(ctx, tx, store.Connection{ID: connID, Name: orgName, OrgURL: a.Org.Domain}); err != nil {
					return err
				}
			} else {
				connID = ""
			}

			var avail *string
			if a.AvailableBalance != nil {
				v := string(*a.AvailableBalance)
				avail = &v
			}
			balAt := a.BalanceDate.Time()
			if a.BalanceDate.IsZero() {
				balAt = s.Cfg.Now().UTC()
			}
			inserted, err := store.UpsertAccount(ctx, tx, store.Account{
				ID: a.ID, ConnectionID: connID, OrgName: orgName, Name: a.Name,
				Currency: defaultStr(a.Currency, "USD"), Balance: string(a.Balance),
				AvailableBalance: avail, BalanceAt: balAt, UpdateBalance: current,
			})
			if err != nil {
				return fmt.Errorf("account %s: %w", a.ID, err)
			}
			if inserted {
				res.NewAccounts++
			}

			seen := make([]string, 0, len(a.Transactions))
			for _, t := range a.Transactions {
				returned++
				seen = append(seen, t.ID)
				row := store.Transaction{
					AccountID: a.ID, ID: t.ID, Amount: string(t.Amount),
					Description: t.Description, Payee: t.Payee, Memo: t.Memo, Pending: t.Pending,
				}
				if !t.Posted.IsZero() {
					p := t.Posted.Time()
					row.PostedAt = &p
				} else {
					row.Pending = true // posted=0 means not yet posted
				}
				if t.TransactedAt != nil && !t.TransactedAt.IsZero() {
					ta := t.TransactedAt.Time()
					row.TransactedAt = &ta
				}
				if err := store.UpsertTransaction(ctx, tx, row); err != nil {
					return fmt.Errorf("transaction %s/%s: %w", a.ID, t.ID, err)
				}
				res.TxnsSeen++
			}

			if current {
				n, err := store.DeleteStalePending(ctx, tx, a.ID, seen)
				if err != nil {
					return err
				}
				res.StaleDeleted += int(n)
				asOf := balAt.In(s.Cfg.Location)
				if err := store.UpsertSnapshot(ctx, tx, a.ID, asOf, string(a.Balance), avail, balAt); err != nil {
					return fmt.Errorf("snapshot %s: %w", a.ID, err)
				}
				res.Snapshots++
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store window %s..%s: %w", start.Format(time.DateOnly), end.Format(time.DateOnly), err)
	}
	s.Log.Info("window synced", "start", start.Format(time.DateOnly), "end", end.Format(time.DateOnly),
		"current", current, "accounts", len(set.Accounts), "transactions", returned)
	return returned, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return "unknown"
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
