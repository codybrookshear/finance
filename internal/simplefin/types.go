// Package simplefin is a minimal client for the SimpleFIN protocol
// (https://www.simplefin.org/protocol.html), written to keep the access
// credentials out of URLs, logs and error messages.
package simplefin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// AccountSet is the response body of GET /accounts.
type AccountSet struct {
	// Errors is the v1 error list: human-readable strings.
	Errors []string `json:"errors"`
	// ErrList is the v2 structured error list.
	ErrList []APIError `json:"errlist"`
	// Connections is present in protocol v2.
	Connections []Connection `json:"connections"`
	Accounts    []Account    `json:"accounts"`
}

// Messages returns all server-reported problems as plain strings.
func (s *AccountSet) Messages() []string {
	out := append([]string(nil), s.Errors...)
	for _, e := range s.ErrList {
		out = append(out, e.String())
	}
	return out
}

type APIError struct {
	Code      string `json:"code"`
	Msg       string `json:"msg"`
	ConnID    string `json:"conn_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

func (e APIError) String() string {
	if e.Code == "" {
		return e.Msg
	}
	return e.Code + ": " + e.Msg
}

type Connection struct {
	ConnID string `json:"conn_id"`
	Name   string `json:"name"`
	OrgID  string `json:"org_id"`
	OrgURL string `json:"org_url"`
}

// Org is the v1 per-account organization object.
type Org struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
	ID     string `json:"id"`
}

type Account struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	ConnID           string        `json:"conn_id"` // v2
	Org              *Org          `json:"org"`     // v1
	Currency         string        `json:"currency"`
	Balance          Decimal       `json:"balance"`
	AvailableBalance *Decimal      `json:"available-balance"`
	BalanceDate      UnixTime      `json:"balance-date"`
	Transactions     []Transaction `json:"transactions"`
}

type Transaction struct {
	ID           string    `json:"id"`
	Posted       UnixTime  `json:"posted"` // zero for many pending transactions
	Amount       Decimal   `json:"amount"`
	Description  string    `json:"description"`
	Payee        string    `json:"payee"` // SimpleFIN Bridge extension
	Memo         string    `json:"memo"`  // SimpleFIN Bridge extension
	TransactedAt *UnixTime `json:"transacted_at"`
	Pending      bool      `json:"pending"`
}

// Decimal is a validated decimal number kept as a string so money never
// passes through float64.
type Decimal string

var decimalRE = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

func (d *Decimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Be lenient: some servers send bare numbers. json.Number keeps
		// the exact text rather than rounding through float64.
		var n json.Number
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("decimal: %w", err)
		}
		s = n.String()
	}
	if !decimalRE.MatchString(s) {
		return fmt.Errorf("decimal: invalid value %q", s)
	}
	*d = Decimal(s)
	return nil
}

// UnixTime is a SimpleFIN timestamp (seconds since epoch).
type UnixTime int64

func (t UnixTime) Time() time.Time { return time.Unix(int64(t), 0).UTC() }
func (t UnixTime) IsZero() bool    { return t == 0 }
