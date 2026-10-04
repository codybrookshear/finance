// Command web serves the read-only UI.
//
// It listens on a Unix socket (WEB_SOCKET) so the container needs no published
// port and no network beyond the internal database one; cloudflared on the
// host, or an SSH tunnel in development, connects to the socket. WEB_ADDR
// (TCP) is for running outside Docker.
//
// Production checks Cloudflare Access on every request:
//
//	ACCESS_TEAM_DOMAIN     <team>.cloudflareaccess.com
//	ACCESS_AUD             the Access application's AUD tag
//	ACCESS_ALLOWED_EMAILS  comma-separated (or ACCESS_ALLOWED_EMAILS_FILE)
//	ACCESS_CERTS_FILE      Access's signing keys (JSON), kept fresh by the host
//
// WEB_DEV_EMAIL instead skips Access and treats every request as that user.
// Development only, on a socket or port nobody else can reach.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"finance/internal/config"
	"finance/internal/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("web failed", "err", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loc, err := config.Location("FINANCE_TZ")
	if err != nil {
		return err
	}
	cfg := web.Config{Location: loc, Log: log}
	if dev := os.Getenv("WEB_DEV_EMAIL"); dev != "" {
		if os.Getenv("ACCESS_AUD") != "" {
			return errors.New("WEB_DEV_EMAIL and ACCESS_* are mutually exclusive")
		}
		cfg.DevEmail = dev
		log.Warn("DEVELOPMENT MODE: Cloudflare Access is not checked; every request is " + dev)
	} else if cfg.Access, err = accessFromEnv(); err != nil {
		return err
	}

	pool, err := config.Pool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	cfg.DB = &web.DB{Pool: pool}

	srv, err := web.New(cfg)
	if err != nil {
		return err
	}
	ln, where, err := listen()
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	log.Info("listening", "on", where)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

// listen opens WEB_SOCKET if set, else WEB_ADDR (default 127.0.0.1:8080).
func listen() (net.Listener, string, error) {
	sock := os.Getenv("WEB_SOCKET")
	if sock == "" {
		addr := os.Getenv("WEB_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		ln, err := net.Listen("tcp", addr)
		return ln, addr, err
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, "", err
	}
	// Who may connect is decided by the directory the socket lives in (on the
	// host): the socket itself is open to anyone who can reach it there.
	if err := os.Chmod(sock, 0o666); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, "unix:" + sock, nil
}

func accessFromEnv() (*web.Access, error) {
	team := os.Getenv("ACCESS_TEAM_DOMAIN")
	aud := os.Getenv("ACCESS_AUD")
	certs := os.Getenv("ACCESS_CERTS_FILE")
	// ACCESS_ALLOWED_EMAILS_FILE in production: kept in 1Password, out of the repo.
	allowed, _ := config.Secret("ACCESS_ALLOWED_EMAILS")
	var emails []string
	for _, e := range strings.Split(allowed, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			emails = append(emails, e)
		}
	}
	if team == "" || aud == "" || certs == "" || len(emails) == 0 {
		return nil, errors.New("set ACCESS_TEAM_DOMAIN, ACCESS_AUD, ACCESS_ALLOWED_EMAILS and ACCESS_CERTS_FILE " +
			"(or WEB_DEV_EMAIL for local development)")
	}
	if !strings.HasSuffix(team, ".cloudflareaccess.com") || strings.Contains(team, "/") {
		return nil, fmt.Errorf("ACCESS_TEAM_DOMAIN should look like <team>.cloudflareaccess.com, got %q", team)
	}
	keys := &web.FileKeys{Path: certs}
	if err := keys.Check(); err != nil { // fail at startup, not on the first request
		return nil, err
	}
	return &web.Access{TeamDomain: team, Audience: aud, AllowedEmails: emails, Keys: keys}, nil
}
