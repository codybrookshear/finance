package manual

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manual.json")
	if err := os.WriteFile(path, []byte(`[{"id":"home","name":"Home","group":"Real estate","value":"500000.00","as_of":"2026-10-03"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if accts, err := Load(path); err != nil || len(accts) != 1 {
		t.Fatalf("%+v %v", accts, err)
	}
	if _, err := Load(path + ".missing"); err == nil {
		t.Error("a missing file should be an error")
	}
}

func TestParse(t *testing.T) {
	ok := `[{"id":"home","name":"Home","group":"Real estate","value":"650000.00","as_of":"2026-10-03"},
	        {"id":"car-loan","name":"Car loan","group":"Debts","value":"-12000","as_of":"2026-10-01","note":"x"}]`
	accts, err := Parse([]byte(ok))
	if err != nil || len(accts) != 2 || accts[1].Value != "-12000" {
		t.Fatalf("%+v %v", accts, err)
	}
	for name, bad := range map[string]string{
		"not json":      `{`,
		"unknown field": `[{"id":"home","name":"Home","group":"G","value":"1","as_of":"2026-10-03","vaule":"2"}]`,
		"bad id":        `[{"id":"Home!","name":"Home","group":"G","value":"1","as_of":"2026-10-03"}]`,
		"duplicate id":  `[{"id":"a","name":"A","group":"G","value":"1","as_of":"2026-10-03"},{"id":"a","name":"B","group":"G","value":"1","as_of":"2026-10-03"}]`,
		"no name":       `[{"id":"a","name":" ","group":"G","value":"1","as_of":"2026-10-03"}]`,
		"no group":      `[{"id":"a","name":"A","value":"1","as_of":"2026-10-03"}]`,
		"commas":        `[{"id":"a","name":"A","group":"G","value":"650,000","as_of":"2026-10-03"}]`,
		"float":         `[{"id":"a","name":"A","group":"G","value":"6.5e5","as_of":"2026-10-03"}]`,
		"dollar sign":   `[{"id":"a","name":"A","group":"G","value":"$650000","as_of":"2026-10-03"}]`,
		"bad date":      `[{"id":"a","name":"A","group":"G","value":"1","as_of":"10/03/2026"}]`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "manual account") {
			t.Errorf("%s: error %q doesn't say where", name, err)
		}
	}
}
