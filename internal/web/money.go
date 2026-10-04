package web

import (
	"regexp"
	"strings"
)

// Amounts arrive from Postgres as text already rounded to cents
// (round(x, 2)::text), so formatting is string work: no float64 anywhere.
var decimalRE = regexp.MustCompile(`^(-?)(\d+)(?:\.(\d{1,2}))?$`)

// formatMoney renders "-1234.5" as "−$1,234.50" (USD) or "−1,234.50 EUR".
// Anything that isn't a plain decimal is returned unchanged.
func formatMoney(amount, currency string) string {
	m := decimalRE.FindStringSubmatch(amount)
	if m == nil {
		return amount
	}
	sign, whole, cents := m[1], strings.TrimLeft(m[2], "0"), m[3]
	if whole == "" {
		whole = "0"
	}
	cents += strings.Repeat("0", 2-len(cents))
	if whole == "0" && cents == "00" {
		sign = ""
	}
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	num := b.String() + "." + cents
	if sign != "" {
		sign = "−" // U+2212, a real minus sign
	}
	if currency == "" || currency == "USD" {
		return sign + "$" + num
	}
	return sign + num + " " + currency
}

// isNegative reports whether a decimal string is below zero.
func isNegative(amount string) bool {
	m := decimalRE.FindStringSubmatch(amount)
	return m != nil && m[1] == "-" && strings.Trim(m[2]+m[3], "0") != ""
}
