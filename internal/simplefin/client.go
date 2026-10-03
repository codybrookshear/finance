package simplefin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBody = 32 << 20 // 32 MiB; generous for years of transactions

// ErrAuth means the access URL was rejected (revoked, or wrong credentials).
var ErrAuth = errors.New("simplefin: access denied (credentials revoked or invalid)")

// ErrPayment means the SimpleFIN Bridge subscription needs attention.
var ErrPayment = errors.New("simplefin: payment required")

// Client talks to a SimpleFIN server. The username and password from the
// access URL are held separately from the base URL, so the URL that
// appears in any net/http error never contains them.
type Client struct {
	base *url.URL // no userinfo
	user string
	pass string
	http *http.Client
}

// Options tune a Client. The zero value is fine for production.
type Options struct {
	HTTPClient *http.Client
	// AllowInsecureLocalhost permits http:// to 127.0.0.1/localhost, for tests only.
	AllowInsecureLocalhost bool
}

// New parses an access URL of the form https://user:pass@host/path.
// Errors deliberately never include the URL itself.
func New(accessURL string, opt Options) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(accessURL))
	if err != nil {
		return nil, errors.New("simplefin: access URL is not a valid URL")
	}
	if err := checkScheme(u, opt.AllowInsecureLocalhost); err != nil {
		return nil, err
	}
	if u.User == nil {
		return nil, errors.New("simplefin: access URL has no credentials")
	}
	pass, ok := u.User.Password()
	if !ok || u.User.Username() == "" {
		return nil, errors.New("simplefin: access URL credentials are incomplete")
	}
	c := &Client{user: u.User.Username(), pass: pass, http: opt.HTTPClient}
	u.User = nil
	u.RawQuery, u.Fragment = "", ""
	c.base = u
	if c.http == nil {
		c.http = &http.Client{Timeout: 2 * time.Minute}
	}
	return c, nil
}

func checkScheme(u *url.URL, allowLocal bool) error {
	if u.Scheme == "https" && u.Host != "" {
		return nil
	}
	if allowLocal && u.Scheme == "http" {
		h := u.Hostname()
		if h == "127.0.0.1" || h == "localhost" || h == "::1" {
			return nil
		}
	}
	return errors.New("simplefin: URL must use https")
}

// String and LogValue keep the credentials out of fmt and slog output.
func (c *Client) String() string { return "simplefin.Client{" + c.base.Redacted() + "}" }
func (c *Client) LogValue() slog.Value {
	return slog.StringValue(c.String())
}

// Query selects what GET /accounts returns.
type Query struct {
	Start, End   time.Time // zero = unset
	Pending      bool
	BalancesOnly bool
	AccountIDs   []string
}

// Accounts calls GET /accounts.
func (c *Client) Accounts(ctx context.Context, q Query) (*AccountSet, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + "/accounts"
	v := url.Values{}
	if !q.Start.IsZero() {
		v.Set("start-date", strconv.FormatInt(q.Start.Unix(), 10))
	}
	if !q.End.IsZero() {
		v.Set("end-date", strconv.FormatInt(q.End.Unix(), 10))
	}
	if q.Pending {
		v.Set("pending", "1")
	}
	if q.BalancesOnly {
		v.Set("balances-only", "1")
	}
	for _, id := range q.AccountIDs {
		v.Add("account", id)
	}
	u.RawQuery = v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("simplefin: build request: %w", err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("simplefin: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrAuth
	case resp.StatusCode == http.StatusPaymentRequired:
		return nil, ErrPayment
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("simplefin: unexpected HTTP status %d", resp.StatusCode)
	}

	var set AccountSet
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBody))
	if err := dec.Decode(&set); err != nil {
		return nil, fmt.Errorf("simplefin: decode response: %w", err)
	}
	return &set, nil
}

// Claim exchanges a one-time setup token for an access URL.
// The returned string is a secret: store it, never log it.
func Claim(ctx context.Context, setupToken string, opt Options) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(setupToken))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(strings.TrimSpace(setupToken))
		if err != nil {
			return "", errors.New("simplefin: setup token is not valid base64")
		}
	}
	claimURL, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", errors.New("simplefin: setup token does not contain a valid URL")
	}
	if err := checkScheme(claimURL, opt.AllowInsecureLocalhost); err != nil {
		return "", err
	}
	hc := opt.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("simplefin: build claim request: %w", err)
	}
	req.Header.Set("Content-Length", "0")
	resp, err := hc.Do(req)
	if err != nil {
		// The claim URL embeds the one-time token; strip it from the error.
		var ue *url.Error
		if errors.As(err, &ue) {
			return "", fmt.Errorf("simplefin: claim request failed: %v", ue.Err)
		}
		return "", errors.New("simplefin: claim request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return "", errors.New("simplefin: setup token was already claimed or is invalid (generate a new one)")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("simplefin: claim returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", errors.New("simplefin: reading claim response failed")
	}
	access := strings.TrimSpace(string(body))
	if _, err := New(access, opt); err != nil {
		return "", fmt.Errorf("simplefin: claim returned an unusable access URL: %w", err)
	}
	return access, nil
}
