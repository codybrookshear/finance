package simplefin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const secretPass = "s3cr3t-pa55w0rd" // gitleaks:allow

func newTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	access := strings.Replace(srv.URL, "http://", "http://user:"+secretPass+"@", 1) + "/simplefin"
	return srv, access
}

func TestNewRejectsPlainHTTPAndMissingCreds(t *testing.T) {
	for _, u := range []string{
		"http://user:pw@bridge.example/simplefin", // http to non-local
		"https://bridge.example/simplefin",        // no creds
		"https://user@bridge.example/simplefin",   // no password
		"::not a url",
	} {
		if _, err := New(u, Options{}); err == nil {
			t.Errorf("New(%q) succeeded, want error", u)
		} else if strings.Contains(err.Error(), "pw") {
			t.Errorf("error leaks URL: %v", err)
		}
	}
}

func TestAccountsSendsBasicAuthAndParses(t *testing.T) {
	_, access := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != secretPass {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/simplefin/accounts" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("pending") != "1" || r.URL.Query().Get("start-date") != "1700000000" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"errors":["Connection to Example Bank may need attention"],
		  "accounts":[{"id":"A1","name":"Checking","currency":"USD",
		    "org":{"domain":"bank.example","name":"Example Bank"},
		    "balance":"1234.56","available-balance":"1200.00","balance-date":1700086400,
		    "transactions":[
		      {"id":"T1","posted":1700000100,"amount":"-12.34","description":"COFFEE","payee":"Cafe"},
		      {"id":"T2","posted":0,"amount":"-5.00","description":"PENDING THING","pending":true,"transacted_at":1700000200}
		    ]}]}`)
	})
	c, err := New(access, Options{AllowInsecureLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	set, err := c.Accounts(context.Background(), Query{Start: time.Unix(1700000000, 0), Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Accounts) != 1 || len(set.Accounts[0].Transactions) != 2 {
		t.Fatalf("unexpected shape: %+v", set)
	}
	a := set.Accounts[0]
	if a.Balance != "1234.56" || *a.AvailableBalance != "1200.00" || a.Org.Name != "Example Bank" {
		t.Errorf("account = %+v", a)
	}
	if tx := a.Transactions[1]; !tx.Pending || !tx.Posted.IsZero() || tx.TransactedAt == nil {
		t.Errorf("pending txn = %+v", tx)
	}
	if got := set.Messages(); len(got) != 1 {
		t.Errorf("messages = %v", got)
	}
}

func TestErrorsNeverContainPassword(t *testing.T) {
	// 1. Server returns 403.
	_, access := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	c, _ := New(access, Options{AllowInsecureLocalhost: true})
	_, err := c.Accounts(context.Background(), Query{})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}

	// 2. Connection refused: net/http includes the request URL in the error.
	srv, access2 := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()
	c2, _ := New(access2, Options{AllowInsecureLocalhost: true})
	_, err = c2.Accounts(context.Background(), Query{})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretPass) {
		t.Fatalf("error leaks password: %v", err)
	}

	// 3. Malformed JSON.
	_, access3 := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{bad") })
	c3, _ := New(access3, Options{AllowInsecureLocalhost: true})
	if _, err := c3.Accounts(context.Background(), Query{}); err == nil || strings.Contains(err.Error(), secretPass) {
		t.Fatalf("err = %v", err)
	}
}

func TestClientFormattingIsRedacted(t *testing.T) {
	c, err := New("https://user:"+secretPass+"@bridge.example/simplefin", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "client", c)
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %s", c, c), buf.String()} {
		if strings.Contains(s, secretPass) {
			t.Fatalf("formatted client leaks password: %s", s)
		}
	}
}

func TestDecimalRejectsGarbage(t *testing.T) {
	var d Decimal
	for _, in := range []string{`"12.5"`, `"-0.01"`, `42`, `"1000000000000.123456"`} {
		if err := d.UnmarshalJSON([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
	for _, in := range []string{`"1e5"`, `"abc"`, `"1,000.00"`, `""`, `"NaN"`} {
		if err := d.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestClaim(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/claim/TOKEN123" {
			http.Error(w, "bad", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, strings.Replace(srvURL, "http://", "http://u:"+secretPass+"@", 1)+"/simplefin\n")
	}))
	defer srv.Close()
	srvURL = srv.URL
	opt := Options{AllowInsecureLocalhost: true}

	token := base64.StdEncoding.EncodeToString([]byte(srv.URL + "/claim/TOKEN123"))
	got, err := Claim(context.Background(), token, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "/simplefin") || !strings.Contains(got, secretPass) {
		t.Errorf("access URL = %q", got)
	}

	bad := base64.StdEncoding.EncodeToString([]byte(srv.URL + "/claim/USED"))
	if _, err := Claim(context.Background(), bad, opt); err == nil || strings.Contains(err.Error(), "USED") {
		t.Errorf("err = %v", err)
	}

	insecure := base64.StdEncoding.EncodeToString([]byte("http://bridge.example/claim/X"))
	if _, err := Claim(context.Background(), insecure, Options{}); err == nil {
		t.Error("accepted non-https claim URL")
	}
}
