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
