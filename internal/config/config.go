// Package config reads settings from the environment. Secrets can be given
// either directly (FOO) or, preferably in production, as a file path
// (FOO_FILE, e.g. a Docker secret under /run/secrets).
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Secret returns $name, or the trimmed contents of the file at $name_FILE.
func Secret(name string) (string, error) {
	if p := os.Getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("reading %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s (or %s_FILE) is not set", name, name)
}

func Int(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func Location(name string) (*time.Location, error) {
	v := os.Getenv(name)
	if v == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(v)
}

// Pool connects to Postgres. Connection parameters come from the standard
// PG* variables (PGHOST, PGUSER, PGDATABASE, PGSSLMODE, ...) or DATABASE_URL;
// the password comes from DB_PASSWORD / DB_PASSWORD_FILE when set, so it
// never has to be embedded in a URL.
func Pool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, errors.New("invalid database configuration") // don't echo a URL that may hold a password
	}
	if pw, err := Secret("DB_PASSWORD"); err == nil {
		cfg.ConnConfig.Password = pw
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
