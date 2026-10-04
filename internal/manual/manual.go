// Package manual holds balances you keep by hand, like your home's estimated
// value. They're personal, so they live in 1Password (env/prod.env's
// MANUAL_ACCOUNTS ref), not in the repo: the deploy writes them to a secret
// file the sync reads (MANUAL_ACCOUNTS_FILE). To change one, edit the item in
// 1Password and redeploy. Each sync writes them like bank accounts, so they
// count in net worth and get a daily balance snapshot. A debt is negative.
//
//	[
//	  {"id": "home", "name": "Home", "group": "Real estate",
//	   "value": "500000.00", "as_of": "2026-10-03", "note": "Zillow Zestimate"}
//	]
//
// Removing an entry doesn't delete the account; set its value to 0 instead.
package manual

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type Account struct {
	ID    string `json:"id"`    // stable: the account is stored as "manual:<id>"
	Name  string `json:"name"`  // shown on the Accounts page
	Group string `json:"group"` // the heading it's listed under
	Value string `json:"value"` // decimal, e.g. "500000.00"; negative for a debt
	AsOf  string `json:"as_of"` // YYYY-MM-DD: when you last updated the value
	Note  string `json:"note"`  // where the number came from (for you; not shown)
}

var (
	idRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	valueRE = regexp.MustCompile(`^-?\d{1,12}(\.\d{1,2})?$`)
)

// Load reads the accounts from a JSON file, or returns an error naming the
// first problem, so a typo stops the sync instead of writing a wrong balance.
func Load(path string) ([]Account, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manual accounts: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("manual accounts: %w", err)
	}
	return Parse(b)
}

func Parse(b []byte) ([]Account, error) {
	var accts []Account
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&accts); err != nil {
		return nil, fmt.Errorf("manual accounts: %w", err)
	}
	seen := map[string]bool{}
	for i, a := range accts {
		switch {
		case !idRE.MatchString(a.ID):
			return nil, fmt.Errorf("manual account %d: id %q must be lowercase letters, digits and dashes", i+1, a.ID)
		case seen[a.ID]:
			return nil, fmt.Errorf("manual account %q: duplicate id", a.ID)
		case strings.TrimSpace(a.Name) == "" || len(a.Name) > 100:
			return nil, fmt.Errorf("manual account %q: name must be 1-100 characters", a.ID)
		case strings.TrimSpace(a.Group) == "" || len(a.Group) > 100:
			return nil, fmt.Errorf("manual account %q: group must be 1-100 characters", a.ID)
		case !valueRE.MatchString(a.Value):
			return nil, fmt.Errorf("manual account %q: value %q isn't a plain decimal like 500000.00", a.ID, a.Value)
		}
		if _, err := time.Parse(time.DateOnly, a.AsOf); err != nil {
			return nil, fmt.Errorf("manual account %q: as_of %q must be YYYY-MM-DD", a.ID, a.AsOf)
		}
		seen[a.ID] = true
	}
	return accts, nil
}
