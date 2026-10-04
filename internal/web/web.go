// Package web is the UI: transaction search and categorizing, monthly
// spending, and accounts with net worth, server-rendered with html/template plus vendored htmx and uPlot.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed templates static
var files embed.FS

// Config wires up a Server. Exactly one of Access and DevEmail is set.
type Config struct {
	DB       *DB
	Access   *Access // production: verify Cloudflare Access on every request
	DevEmail string  // local development only: skip Access, act as this user
	Location *time.Location
	Log      *slog.Logger
}

type Server struct {
	cfg     Config
	pages   map[string]*template.Template
	static  http.Handler
	version string // content hash of static/, for cache-busting URLs
}

func New(cfg Config) (*Server, error) {
	if (cfg.Access == nil) == (cfg.DevEmail == "") {
		return nil, errors.New("web: set exactly one of Access and DevEmail")
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	staticFS, err := fs.Sub(files, "static")
	if err != nil {
		return nil, err
	}
	version, err := hashFS(staticFS)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, static: http.FileServerFS(staticFS), version: version}
	s.pages = map[string]*template.Template{}
	for _, page := range []string{"transactions", "spending", "accounts"} {
		t, err := template.New("layout.html").Funcs(s.funcs()).
			ParseFS(files, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("web: template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

// Handler returns the app with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// GET patterns also match HEAD; other methods get 405. The POST routes
	// are edits; CrossOriginProtection (below) refuses them from any other
	// site.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/transactions", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /transactions", s.transactions)
	mux.HandleFunc("GET /spending", s.spending)
	mux.HandleFunc("GET /accounts", s.accounts)
	mux.HandleFunc("GET /networth", func(w http.ResponseWriter, r *http.Request) { // the page's old name
		http.Redirect(w, r, "/accounts", http.StatusMovedPermanently)
	})
	mux.HandleFunc("POST /transactions/category", s.setCategory)
	mux.HandleFunc("POST /transactions/note", s.setNote)
	mux.HandleFunc("GET /categories.css", s.categoriesCSS)
	mux.HandleFunc("GET /static/", s.serveStatic)
	csrf := http.NewCrossOriginProtection() // Sec-Fetch-Site / Origin checks on non-GET requests
	return s.securityHeaders(s.logRequests(s.recoverPanics(s.authenticate(csrf.Handler(mux)))))
}

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

func userEmail(ctx context.Context) string { s, _ := ctx.Value(ctxKey{}).(string); return s }

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := s.cfg.DevEmail
		if s.cfg.Access != nil {
			var err error
			if email, err = s.cfg.Access.Verify(r.Header.Get(accessHeader)); err != nil {
				s.cfg.Log.Warn("access denied", "path", r.URL.Path, "reason", err.Error())
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, email)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// logRequests logs method, path and status. Never the query string: it holds
// search terms, which are financial details.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		s.cfg.Log.Info("request", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.cfg.Log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(v))
				http.Error(w, "Something went wrong.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// categoriesCSS gives each category's chip its color: per-category classes,
// since the CSP allows no inline styles. Tiny, so it isn't cached.
func (s *Server) categoriesCSS(w http.ResponseWriter, r *http.Request) {
	cats, err := s.cfg.DB.Categories(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var b strings.Builder
	for _, c := range cats {
		if c.Hue == nil {
			fmt.Fprintf(&b, ".c-%d{--chip-s:0%%}\n", c.ID)
		} else {
			fmt.Fprintf(&b, ".c-%d{--chip-h:%d}\n", c.ID, *c.Hue)
		}
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	io.WriteString(w, b.String())
}

// serveStatic serves only .js and .css files. URLs carry ?v=<content hash>,
// so they can be cached privately (never by Cloudflare: "private").
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	switch path.Ext(r.URL.Path) {
	case ".js", ".css":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.StripPrefix("/static", s.static).ServeHTTP(w, r)
}

// render executes into a buffer first, so a template error becomes a clean
// 500 instead of a truncated page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page, block string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, block, data); err != nil {
		s.fail(w, r, fmt.Errorf("render %s/%s: %w", page, block, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Log.Error("request failed", "path", r.URL.Path, "err", err.Error())
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"money":  formatMoney,
		"commas": commas,
		// withCats pairs a row with the category list for its picker.
		"withCats": func(t Txn, cats []Category) any {
			return struct {
				Txn
				Categories []Category
			}{t, cats}
		},
		"neg":   isNegative,
		"asset": func(name string) string { return "/static/" + name + "?v=" + s.version },
		"day": func(t time.Time) string {
			t = t.In(s.cfg.Location)
			if t.Year() == time.Now().In(s.cfg.Location).Year() {
				return t.Format("Jan 2")
			}
			return t.Format("Jan 2, 2006")
		},
		"stamp": func(t *time.Time) string {
			if t == nil {
				return "never"
			}
			return t.In(s.cfg.Location).Format("Jan 2, 3:04 PM")
		},
		"monthName": func(ym string) string {
			t, err := time.Parse("2006-01", ym)
			if err != nil {
				return ym
			}
			return t.Format("January 2006")
		},
	}
}

func hashFS(fsys fs.FS) (string, error) {
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
