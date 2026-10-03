// Command migrate applies database migrations. Run it as the schema owner
// role; the app roles (finance_sync, finance_web) cannot change the schema.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"finance/internal/config"
	"finance/internal/store"
	"finance/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := config.Pool(ctx)
	if err != nil {
		log.Error("migrate failed", "err", err.Error())
		os.Exit(1)
	}
	defer pool.Close()

	applied, err := store.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		log.Error("migrate failed", "err", err.Error(), "applied", applied)
		os.Exit(1)
	}
	log.Info("migrations up to date", "applied", applied)
}
