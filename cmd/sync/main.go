// Command sync pulls bank data from SimpleFIN into Postgres.
//
//	sync run     one sync pass (default); schedule it a few times a day
//	sync claim   read a SimpleFIN setup token on stdin, print the access URL
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"finance/internal/config"
	"finance/internal/manual"
	"finance/internal/simplefin"
	"finance/internal/store"
	"finance/internal/syncer"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "run":
		err = run(ctx, log)
	case "claim":
		err = claim(ctx, os.Stdin, os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q (want run or claim)", cmd)
	}
	if err != nil {
		log.Error("sync failed", "err", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	accessURL, err := config.Secret("SIMPLEFIN_ACCESS_URL")
	if err != nil {
		return err
	}
	client, err := simplefin.New(accessURL, simplefin.Options{})
	if err != nil {
		return err
	}
	accessURL = "" // drop our copy; the client holds what it needs

	cfg := syncer.DefaultConfig()
	if cfg.Location, err = config.Location("FINANCE_TZ"); err != nil {
		return err
	}
	if p := os.Getenv("MANUAL_ACCOUNTS_FILE"); p != "" { // production: from 1Password
		if cfg.Manual, err = manual.Load(p); err != nil {
			return err
		}
	}
	if cfg.MaxBackfillPerRun, err = config.Int("SYNC_MAX_BACKFILL_REQUESTS", cfg.MaxBackfillPerRun); err != nil {
		return err
	}
	days, err := config.Int("SYNC_MAX_LOOKBACK_DAYS", int(cfg.MaxLookback.Hours()/24))
	if err != nil {
		return err
	}
	cfg.MaxLookback = time.Duration(days) * 24 * time.Hour

	pool, err := config.Pool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	s := &syncer.Syncer{Store: &store.Store{Pool: pool}, Client: client, Cfg: cfg, Log: log}
	res, err := s.Run(ctx)
	if err != nil {
		return err
	}
	log.Info("sync complete", "requests", res.Requests, "new_accounts", res.NewAccounts, "transactions_seen", res.TxnsSeen,
		"stale_pending_deleted", res.StaleDeleted, "snapshots", res.Snapshots)

	if n, err := s.Store.MarkDuplicateAccounts(ctx); err != nil {
		return fmt.Errorf("mark duplicate accounts: %w", err)
	} else if n > 0 {
		log.Info("duplicate accounts marked or cleared", "accounts", n)
	}

	c, err := s.Store.Categorize(ctx)
	if err != nil {
		return fmt.Errorf("categorize: %w", err)
	}
	log.Info("categorized", "guessed", c.Learned, "transfers", c.Transfers)
	return nil
}

// claim reads the setup token from stdin (never argv, which lands in shell
// history and process listings) and writes the access URL to stdout so it
// can be piped straight into a secret store.
func claim(ctx context.Context, in io.Reader, out io.Writer) error {
	if f, ok := in.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(os.Stderr, "Paste SimpleFIN setup token, then press Enter: ")
		}
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return fmt.Errorf("no setup token given on stdin")
	}
	access, err := simplefin.Claim(ctx, token, simplefin.Options{})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, access)
	return err
}
