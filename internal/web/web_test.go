package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"finance/internal/store"
)

// The DB-backed tests need a migrated database (make test provides one). They
// read as finance_web, so they also prove its read-only grants are enough:
//
//	TEST_WEB_DATABASE_URL=postgres://finance_web@127.0.0.1:5432/finance \
//	TEST_OWNER_DATABASE_URL=postgres://finance_owner@127.0.0.1:5432/finance go test ./internal/web

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var la = func() *time.Location {
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return l
}()

func TestVendoredAssets(t *testing.T) {
	// Must match static/vendor/README.md. A change here means someone edited
	// a vendored file: re-verify it against npm before updating these.
	want := map[string]string{
		"static/vendor/htmx.min.js":       "71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de",
		"static/vendor/uPlot.iife.min.js": "19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f",
		"static/vendor/uPlot.min.css":     "df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04",
	}
	for name, sum := range want {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != sum {
			t.Errorf("%s: sha256 changed", name)
		}
	}
}

func get(t *testing.T, h http.Handler, target string, hdr ...string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAccessRequiredEverywhere(t *testing.T) {
	k := testKey(t)
	a, _ := testAccess(t, k)
	a.Now = nil // real clock for the handler
	srv, err := New(Config{DB: &DB{}, Access: a, Location: la, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	for _, p := range []string{"/", "/transactions", "/spending", "/accounts", "/networth", "/categories.css", "/static/app.js", "/nope"} {
		res := get(t, h, p)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s without a token: %d, want 403", p, res.StatusCode)
		}
		if res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s: no CSP on the 403", p)
		}
	}

	claims := goodClaims()
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	claims["nbf"] = time.Now().Add(-time.Minute).Unix()
	claims["iat"] = time.Now().Add(-time.Minute).Unix()
	tok := signRS256(t, k, "k1", claims)
	if res := get(t, h, "/static/app.js", accessHeader, tok); res.StatusCode != http.StatusOK {
		t.Errorf("valid token: %d, want 200", res.StatusCode)
	}
	// The CF_Authorization cookie alone is not accepted; only the header.
	r := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)
	r.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: tok})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("cookie only: %d, want 403", w.Code)
	}
}

func TestHeadersMethodsAndStatic(t *testing.T) {
	srv, err := New(Config{DB: &DB{}, DevEmail: "dev@example.com", Location: la, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	res := get(t, h, "/static/app.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("app.js: %d", res.StatusCode)
	}
	for k, v := range map[string]string{
		"Content-Security-Policy": csp,
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "private, max-age=86400",
	} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP too loose: %s", csp)
	}
	for p, ct := range map[string]string{
		"/static/icon.svg":             "image/svg+xml",
		"/static/icon-180.png":         "image/png",
		"/static/manifest.webmanifest": "application/manifest+json",
	} {
		if res := get(t, h, p); res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != ct {
			t.Errorf("GET %s: %d %q", p, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	for _, p := range []string{"/static/vendor/README.md", "/static/vendor/uPlot.LICENSE", "/static/../web.go"} {
		if res := get(t, h, p); res.StatusCode == http.StatusOK {
			t.Errorf("GET %s: served", p)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(m, "/transactions", strings.NewReader("x=1")))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /transactions: %d, want 405", m, w.Code)
		}
	}
	if res := get(t, h, "/"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/transactions" {
		t.Errorf("GET /: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// seed resets the bank tables and loads a small fixture, relative to now so
// the "last 12 months" views always include it.
func seed(t *testing.T) *DB {
	t.Helper()
	webURL, ownerURL := os.Getenv("TEST_WEB_DATABASE_URL"), os.Getenv("TEST_OWNER_DATABASE_URL")
	if webURL == "" || ownerURL == "" {
		t.Skip("TEST_WEB_DATABASE_URL / TEST_OWNER_DATABASE_URL not set")
	}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, s := range []string{`DELETE FROM balance_snapshots`, `DELETE FROM categorizations`, `DELETE FROM transactions`,
		`DELETE FROM accounts`, `DELETE FROM connections`, `DELETE FROM sync_runs`} {
		exec(s)
	}
	exec(`INSERT INTO accounts (id, org_name, name, display_name, balance, balance_at, hidden, include_in_net_worth) VALUES
		('chk', 'Test Bank', 'CHECKING 1234', 'Checking', 1000.50, now(), false, true),
		('card', 'Test Bank', 'VISA 9876', NULL, -250.25, now(), false, true),
		('old', 'Old Bank', 'Closed', NULL, 999, now(), true, true)`)

	now := time.Now().In(la)
	lastMonth := time.Date(now.Year(), now.Month()-1, 10, 12, 0, 0, 0, la)
	// 60 coffees, one a day backwards from lastMonth: two pages of results.
	for i := 0; i < 60; i++ {
		exec(`INSERT INTO transactions (account_id, id, posted_at, transacted_at, amount, description, payee, memo)
			VALUES ('chk', $1, $2, $2, -4.50, 'Coffee Shop', 'Coffee Shop', 'COFFEE #42')`,
			fmt.Sprintf("c%02d", i), lastMonth.AddDate(0, 0, -i))
	}
	exec(`INSERT INTO transactions (account_id, id, posted_at, transacted_at, amount, description, payee, memo, category_id, is_transfer, pending) VALUES
		('chk', 'pay', $1, $1, 2000.00, 'Payroll', 'Employer', 'DIRECT DEP', NULL, false, false),
		('card', 'groc', $1, $1, -12.34, 'Grocery store', 'Grocer', '', (SELECT id FROM categories WHERE name = 'Groceries'), false, false),
		('card', 'xss', $1, $1, -1.00, '<script>alert(1)</script>', '', '', NULL, false, false),
		('card', 'xfer', $1, $1, -500.00, 'Payment thank you', '', '', NULL, true, false),
		('card', 'pend', NULL, $2, -7.77, 'Pending thing', '', '', NULL, false, true),
		('old', 'secret', $1, $1, -99.00, 'Hidden account txn', '', '', NULL, false, false)`,
		lastMonth.Add(time.Hour), now)
	for i, d := range []int{3, 2, 1} {
		day := now.AddDate(0, 0, -d).Format(time.DateOnly)
		exec(`INSERT INTO balance_snapshots (account_id, as_of, balance, balance_at) VALUES
			('chk', $1, $2, now()), ('card', $1, -100, now()), ('old', $1, 5000, now())`, day, 1000+i*100)
	}
	exec(`INSERT INTO sync_runs (finished_at, status) VALUES (now(), 'ok')`)

	pool, err := pgxpool.New(ctx, webURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool}
}

func TestPages(t *testing.T) {
	db := seed(t)
	srv, err := New(Config{DB: db, DevEmail: "dev@example.com", Location: la, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	page := func(target string, hdr ...string) string {
		t.Helper()
		res := get(t, h, target, hdr...)
		b := body(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d\n%s", target, res.StatusCode, b)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control %q", target, cc)
		}
		return b
	}

	t.Run("transactions", func(t *testing.T) {
		b := page("/transactions")
		for _, want := range []string{"<!doctype html>", "65 transactions", "Coffee Shop", "Payroll",
			"−$4.50", "$2,000.00", "Checking", "VISA 9876", "Pending thing", "dev@example.com",
			`id="more"`, "&lt;script&gt;alert(1)&lt;/script&gt;"} {
			if !strings.Contains(b, want) {
				t.Errorf("missing %q", want)
			}
		}
		for _, bad := range []string{"<script>alert(1)", "Hidden account txn", "Closed"} {
			if strings.Contains(b, bad) {
				t.Errorf("should not contain %q", bad)
			}
		}
		if n := strings.Count(b, "<details>"); n != 50 {
			t.Errorf("first page has %d rows, want 50", n)
		}
	})

	t.Run("infinite scroll", func(t *testing.T) {
		b := page("/transactions")
		m := regexp.MustCompile(`id="more" class="more" hx-get="([^"]+)"`).FindStringSubmatch(b)
		if m == nil {
			t.Fatal("no next-page link")
		}
		next := strings.ReplaceAll(m[1], "&amp;", "&")
		rows := page(next, "HX-Request", "true", "HX-Target", "more")
		if strings.Contains(rows, "<html") || strings.Contains(rows, `role="search"`) {
			t.Error("scroll response should be rows only")
		}
		if n := strings.Count(rows, "<details>"); n != 15 {
			t.Errorf("second page has %d rows, want 15", n)
		}
		if strings.Contains(rows, `id="more"`) {
			t.Error("last page should have no next link")
		}
		if res := get(t, h, "/transactions?after=garbage"); res.StatusCode != http.StatusBadRequest {
			t.Errorf("bad cursor: %d", res.StatusCode)
		}
	})

	t.Run("search", func(t *testing.T) {
		cases := []struct{ q, want, notWant string }{
			{"grocer", "Grocery store", "Coffee Shop"}, // substring of description
			{"COFFEE #42", "Coffee Shop", "Payroll"},   // memo
			{"Employer", "Payroll", "Coffee Shop"},     // payee
			{"12.34", "Grocery store", "Coffee Shop"},  // amount
			{"$2,000", "Payroll", "Coffee Shop"},       // amount, formatted
			{"100%", "0 transactions", "Coffee Shop"},  // LIKE wildcard taken literally
			{"zzz_nothing", "No transactions match", "Coffee Shop"},
		}
		for _, c := range cases {
			b := page("/transactions?q="+url.QueryEscape(c.q), "HX-Request", "true", "HX-Target", "results")
			if strings.Contains(b, "<html") {
				t.Errorf("q=%q: results request returned a full page", c.q)
			}
			if !strings.Contains(b, c.want) || strings.Contains(b, c.notWant) {
				t.Errorf("q=%q: want %q and not %q", c.q, c.want, c.notWant)
			}
		}
	})

	t.Run("filters", func(t *testing.T) {
		if b := page("/transactions?account=card"); strings.Contains(b, "Coffee Shop") || !strings.Contains(b, "4 transactions") {
			t.Error("account filter")
		}
		lm := time.Now().In(la).AddDate(0, -1, 0)
		day := time.Date(lm.Year(), lm.Month(), 10, 0, 0, 0, 0, la).Format(time.DateOnly)
		b := page("/transactions?from=" + day + "&to=" + day + "&q=coffee")
		if !strings.Contains(b, "1 transaction ") || !strings.Contains(b, "clear") {
			t.Errorf("date filter: %s", regexp.MustCompile(`(?s)<p class="summary">.*?</p>`).FindString(b))
		}
		if b := page("/transactions?from=not-a-date"); !strings.Contains(b, "65 transactions") {
			t.Error("a bad date should be ignored")
		}
		// A reversed range is taken the right way round, not as empty.
		first := time.Date(lm.Year(), lm.Month(), 1, 0, 0, 0, 0, la).Format(time.DateOnly)
		b = page("/transactions?from=" + day + "&to=" + first + "&q=coffee")
		if !strings.Contains(b, "10 transactions") || !strings.Contains(b, `name="from" value="`+first+`" max="`+day+`"`) {
			t.Errorf("reversed range: %s", regexp.MustCompile(`(?s)<p class="summary">.*?</p>`).FindString(b))
		}
	})

	t.Run("spending", func(t *testing.T) {
		b := page("/spending")
		lm := time.Now().In(la).AddDate(0, -1, 0)
		// Last month: income 2,000.00; spending 10 coffees (45.00) + 12.34 + 1.00.
		for _, want := range []string{lm.Format("January 2006"), "Groceries", "$12.34", "Uncategorized",
			"income $2,000.00", "spending $58.34", "$1,941.66"} {
			if !strings.Contains(b, want) {
				t.Errorf("missing %q in:\n%s", want, regexp.MustCompile(`(?s)<main>.*</main>`).FindString(b))
				break
			}
		}
		// Rows link to that month's transactions in that category.
		groceries := regexp.MustCompile(`href="(/transactions\?category=\d+&amp;from=[0-9-]+&amp;to=[0-9-]+)"`).FindStringSubmatch(b)
		if groceries == nil {
			t.Fatal("no category link")
		}
		if b := page(strings.ReplaceAll(groceries[1], "&amp;", "&")); !strings.Contains(b, "1 transaction ") || !strings.Contains(b, "Grocer") {
			t.Error("category link")
		}
		none := regexp.MustCompile(`href="(/transactions\?category=none&amp;from=[0-9-]+&amp;to=[0-9-]+)"`).FindStringSubmatch(b)
		if none == nil {
			t.Fatal("no uncategorized link")
		}
		if b := page(strings.ReplaceAll(none[1], "&amp;", "&")); !strings.Contains(b, "Employer") || !strings.Contains(b, "Coffee Shop") {
			t.Error("uncategorized link: money in and out")
		}
		for _, bad := range []string{"Pending", "500.00", "99.00"} { // pending, transfer, hidden
			if strings.Contains(b, bad) {
				t.Errorf("should not count %q", bad)
			}
		}
	})

	t.Run("accounts", func(t *testing.T) {
		if res := get(t, h, "/networth"); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/accounts" {
			t.Errorf("old /networth: %d %s", res.StatusCode, res.Header.Get("Location"))
		}
		b := page("/accounts")
		for _, want := range []string{"<h1>Accounts</h1>", `<p class="label">Net worth</p>`, `aria-current="page">Accounts</a>`} {
			if !strings.Contains(b, want) {
				t.Errorf("missing %q", want)
			}
		}
		// Current: 1000.50 + (-250.25); the hidden account doesn't count.
		for _, want := range []string{"$750.25", "Checking", "−$250.25", `id="networth-chart"`,
			`<li class="group">`, `<span class="what">Test Bank</span>`,
			`data-values="900.00,1000.00,1100.00"`} {
			if !strings.Contains(b, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(b, "Closed") {
			t.Error("hidden account shown")
		}
	})
}

func TestEditing(t *testing.T) {
	db := seed(t)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_OWNER_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	catID := func(name string) string {
		var id int32
		if err := owner.QueryRow(ctx, `SELECT id FROM categories WHERE name = $1`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(id)
	}
	state := func(id string) (category, source, note string) {
		t.Helper()
		if err := owner.QueryRow(ctx, `SELECT coalesce(c.name, ''), coalesce(t.category_source, ''), coalesce(t.note, '')
			FROM transactions t LEFT JOIN categories c ON c.id = t.category_id WHERE t.id = $1`, id).
			Scan(&category, &source, &note); err != nil {
			t.Fatal(err)
		}
		return
	}

	srv, err := New(Config{DB: db, DevEmail: "dev@example.com", Location: la, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	post := func(target string, form url.Values, hdr ...string) *http.Response {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Result()
	}

	t.Run("database permissions", func(t *testing.T) {
		if _, err := db.Pool.Exec(ctx, `UPDATE transactions SET note = 'x'`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("finance_web wrote without SET ROLE: %v", err)
		}
		err := db.edit(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE transactions SET amount = 0 WHERE id = 'c00'`)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("finance_edit changed an amount: %v", err)
		}
	})

	t.Run("chips", func(t *testing.T) {
		b := page(t, h, "/transactions")
		for _, want := range []string{`<li class="day">`, `<select name="category" aria-label="Category: none">`,
			`>🛒 Groceries</option>`, `aria-label="Category: Groceries"`, `href="/categories.css"`} {
			if !strings.Contains(b, want) {
				t.Errorf("missing %q", want)
			}
		}
		css := page(t, h, "/categories.css")
		if !strings.Contains(css, "--chip-h:115") || !strings.Contains(css, "--chip-s:0%") {
			t.Errorf("categories.css:\n%s", css)
		}
	})

	t.Run("similar transactions follow", func(t *testing.T) {
		res := post("/transactions/category", url.Values{"account": {"chk"}, "id": {"c00"}, "category": {catID("Dining")}})
		b := body(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("set category: %d\n%s", res.StatusCode, b)
		}
		if n := strings.Count(b, `hx-swap-oob="true"`); n != 59 {
			t.Errorf("%d rows refreshed, want the 59 newly guessed", n)
		}
		if c, src, _ := state("c00"); c != "Dining" || src != "manual" {
			t.Errorf("c00 = %q %q", c, src)
		}
		if c, src, _ := state("c02"); c != "Dining" || src != "learned" {
			t.Errorf("c02 = %q %q, want a Dining guess", c, src)
		}
		res = post("/transactions/note", url.Values{"account": {"chk"}, "id": {"c00"}, "note": {"Lunch with Sam"}})
		b = body(t, res)
		if res.StatusCode != http.StatusOK || !strings.Contains(b, `<span class="what">Lunch with Sam</span>`) ||
			!strings.Contains(b, "<details open>") || !strings.Contains(b, "<dt>Merchant</dt><dd>Coffee Shop</dd>") {
			t.Errorf("note: %d\n%s", res.StatusCode, b)
		}
		if !strings.Contains(b, `value="Lunch with Sam" placeholder="Coffee Shop"`) {
			t.Error("the name field should show the original name as its placeholder")
		}
		if b := page(t, h, "/transactions?q=sam"); !strings.Contains(b, "1 transaction ") || !strings.Contains(b, "Lunch with Sam") {
			t.Error("search should find notes")
		}
		if c, _, note := state("c00"); c != "Dining" || note != "Lunch with Sam" {
			t.Errorf("after note: %q %q", c, note)
		}
		if b := page(t, h, "/transactions?category=learned"); !strings.Contains(b, "59 transactions") || !strings.Contains(b, "Category: Dining (guess)") {
			t.Error("Guessed filter")
		}
		if b := page(t, h, "/transactions?category=none"); strings.Contains(b, "Coffee Shop") || !strings.Contains(b, "Employer") {
			t.Error("Uncategorized filter")
		}
	})

	t.Run("correcting a guess", func(t *testing.T) {
		// Two equally recent, conflicting choices: no confident guess.
		post("/transactions/category", url.Values{"account": {"chk"}, "id": {"c05"}, "category": {catID("Groceries")}})
		if c, src, _ := state("c02"); c != "" || src != "" {
			t.Errorf("c02 = %q %q, want no guess", c, src)
		}
		var prev, prevSource string
		if err := owner.QueryRow(ctx, `SELECT coalesce(p.name, ''), coalesce(h.previous_source, '')
			FROM categorizations h LEFT JOIN categories p ON p.id = h.previous_category_id
			WHERE h.transaction_id = 'c05' ORDER BY h.id DESC LIMIT 1`).Scan(&prev, &prevSource); err != nil {
			t.Fatal(err)
		}
		if prev != "Dining" || prevSource != "learned" {
			t.Errorf("history: was %q %q, want the Dining guess", prev, prevSource)
		}
		// Back to automatic: c05 and the rest follow the remaining choice.
		post("/transactions/category", url.Values{"account": {"chk"}, "id": {"c05"}, "category": {""}})
		if c, src, _ := state("c05"); c != "Dining" || src != "learned" {
			t.Errorf("c05 = %q %q", c, src)
		}
	})

	t.Run("bad requests", func(t *testing.T) {
		for name, tc := range map[string]struct {
			path string
			form url.Values
			want int
		}{
			"no such category": {"/transactions/category", url.Values{"account": {"chk"}, "id": {"c03"}, "category": {"999999"}}, 400},
			"bad category":     {"/transactions/category", url.Values{"account": {"chk"}, "id": {"c03"}, "category": {"abc"}}, 400},
			"no such txn":      {"/transactions/category", url.Values{"account": {"chk"}, "id": {"nope"}}, 404},
			"hidden account":   {"/transactions/category", url.Values{"account": {"old"}, "id": {"secret"}}, 404},
			"long note":        {"/transactions/note", url.Values{"account": {"chk"}, "id": {"c03"}, "note": {strings.Repeat("x", 501)}}, 400},
			"note, no txn":     {"/transactions/note", url.Values{"account": {"chk"}, "id": {"nope"}, "note": {"x"}}, 404},
		} {
			if res := post(tc.path, tc.form); res.StatusCode != tc.want {
				t.Errorf("%s: %d, want %d", name, res.StatusCode, tc.want)
			}
		}
	})

	t.Run("cross-site requests refused", func(t *testing.T) {
		form := url.Values{"account": {"chk"}, "id": {"c04"}, "category": {catID("Travel")}}
		if res := post("/transactions/category", form, "Sec-Fetch-Site", "cross-site"); res.StatusCode != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site cross-site: %d", res.StatusCode)
		}
		if res := post("/transactions/note", url.Values{"account": {"chk"}, "id": {"c04"}, "note": {"x"}}, "Sec-Fetch-Site", "", "Origin", "https://evil.example"); res.StatusCode != http.StatusForbidden {
			t.Errorf("foreign Origin: %d", res.StatusCode)
		}
		if c, _, _ := state("c04"); c == "Travel" {
			t.Error("a cross-site request changed data")
		}
	})
}

func page(t *testing.T, h http.Handler, target string) string {
	t.Helper()
	res := get(t, h, target)
	b := body(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", target, res.StatusCode, b)
	}
	return b
}

func TestDuplicateAccounts(t *testing.T) {
	db := seed(t)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_OWNER_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// A joint account that two bank logins both report.
	for _, sql := range []string{
		`INSERT INTO connections (id, name, org_url) VALUES
			('v1', 'Login A', 'https://invest.example.com'),
			('v2', 'Login B', 'https://invest.example.com/')`,
		`INSERT INTO accounts (id, connection_id, org_name, name, balance, balance_at, created_at) VALUES
			('joint-a', 'v1', 'Login A', 'Brokerage (1234)', 5000, now(), now() - interval '1 hour'),
			('joint-b', 'v2', 'Login B', 'Brokerage (1234)', 5000.10, now(), now())`,
		`INSERT INTO transactions (account_id, id, posted_at, amount, description) VALUES
			('joint-a', 'div', now(), 12.00, 'DIVIDEND'), ('joint-b', 'div', now(), 12.00, 'DIVIDEND')`,
	} {
		if _, err := owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(Config{DB: db, DevEmail: "dev@example.com", Location: la, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	if b := page(t, h, "/transactions?q=dividend"); !strings.Contains(b, "2 transactions") {
		t.Error("before marking, both copies show")
	}
	if n, err := (&store.Store{Pool: owner}).MarkDuplicateAccounts(ctx); err != nil || n != 1 {
		t.Fatalf("marked %d (%v), want 1", n, err)
	}
	b := page(t, h, "/accounts")
	// Counted once (5,000 + the seed's 750.25), listed once, under the first copy's bank.
	for _, want := range []string{"$5,750.25", "also under Login B", "Login A"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(b, "Brokerage (1234)") != 1 {
		t.Error("the joint account should be listed once")
	}
	if b := page(t, h, "/transactions?q=dividend"); !strings.Contains(b, "1 transaction ") {
		t.Error("the duplicate's transactions should be gone")
	}
}
