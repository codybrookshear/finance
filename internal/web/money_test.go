package web

import "testing"

func TestFormatMoney(t *testing.T) {
	for _, tc := range []struct{ in, cur, want string }{
		{"0", "USD", "$0.00"},
		{"0.00", "USD", "$0.00"},
		{"-0.00", "USD", "$0.00"},
		{"5", "USD", "$5.00"},
		{"5.5", "USD", "$5.50"},
		{"-157.70", "USD", "−$157.70"},
		{"1234.56", "USD", "$1,234.56"},
		{"-1234567.89", "", "−$1,234,567.89"},
		{"100000", "USD", "$100,000.00"},
		{"007.10", "USD", "$7.10"},
		{"12.30", "EUR", "12.30 EUR"},
		{"-12.30", "EUR", "−12.30 EUR"},
		{"1e5", "USD", "1e5"},       // not a plain decimal: unchanged
		{"12.345", "USD", "12.345"}, // more than cents: caller didn't round
		{"", "USD", ""},
	} {
		if got := formatMoney(tc.in, tc.cur); got != tc.want {
			t.Errorf("formatMoney(%q, %q) = %q, want %q", tc.in, tc.cur, got, tc.want)
		}
	}
}

func TestIsNegative(t *testing.T) {
	for in, want := range map[string]bool{
		"-1": true, "-0.01": true, "-0.00": false, "0": false, "3.5": false, "abc": false,
	} {
		if got := isNegative(in); got != want {
			t.Errorf("isNegative(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCents(t *testing.T) {
	for in, want := range map[string]int64{"12": 1200, "12.5": 1250, "-3.07": -307, "0.00": 0, "1234567.89": 123456789} {
		if got, ok := toCents(in); !ok || got != want {
			t.Errorf("toCents(%q) = %d, %v; want %d", in, got, ok, want)
		}
		if got := fromCents(want); got != fromCents(want) || (in == "-3.07" && got != in) {
			t.Errorf("fromCents(%d) = %q", want, got)
		}
	}
	if _, ok := toCents("abc"); ok {
		t.Error("toCents accepted junk")
	}
	if got := fromCents(5); got != "0.05" {
		t.Errorf("fromCents(5) = %q", got)
	}
}

func TestForecast(t *testing.T) {
	last := Month{Income: "2000.00"}
	set := func(v string) *string { return &v }
	f := forecastFor(last, nil, Month{Income: "100.00", Spent: "500.00"})
	if f == nil || f.Expected != "2000.00" || f.Left != "1500.00" || f.LeftNeg || f.Source != "last" {
		t.Errorf("forecast = %+v", f)
	}
	f = forecastFor(last, set("15000.00"), Month{Spent: "500.00"})
	if f.Expected != "15000.00" || f.Left != "14500.00" || f.Source != "set" || f.Setting != "15000.00" {
		t.Errorf("forecast = %+v", f)
	}
	// More has arrived than expected: expect what came.
	f = forecastFor(last, nil, Month{Income: "2500.00", Spent: "3000.00"})
	if f.Expected != "2500.00" || f.Left != "-500.00" || !f.LeftNeg || f.Source != "arrived" {
		t.Errorf("forecast = %+v", f)
	}
	if forecastFor(Month{}, nil, Month{}) != nil {
		t.Error("no last month and nothing set: no forecast")
	}
}

func TestShares(t *testing.T) {
	line := func(amount string) spendLine { return spendLine{Line: Line{Amount: amount}} }
	segs := shares([]spendLine{line("60.00"), line("30.00"), line("10.00"), line("-5.00")})
	if len(segs) != 3 {
		t.Fatalf("segments = %+v", segs)
	}
	if s := segs[0]; s.X != 0 || s.W != 600 || s.Percent != 60 || !s.Icon {
		t.Errorf("first = %+v", s)
	}
	if s := segs[2]; s.X != 900 || s.W != 100 || s.X+s.W != 1000 {
		t.Errorf("last = %+v", s)
	}
	if shares(nil) != nil {
		t.Error("nothing spent, no bar")
	}
}
