// Package moneyfmt formats fixed-point minor-unit amounts for human
// display in `ration status` tables. It never uses float arithmetic,
// matching the Money convention in docs/ARCHITECTURE.md.
package moneyfmt

import (
	"fmt"
	"strings"
)

// Minor formats a minor-units amount (e.g. cents) with its ISO 4217
// currency code as "1,234.56 USD". Negative amounts keep the sign on the
// whole-number part, e.g. "-1,234.56 USD".
func Minor(minorUnits int64, currency string) string {
	sign := ""
	abs := minorUnits
	if abs < 0 {
		sign = "-"
		abs = -abs
	}

	whole := abs / 100
	frac := abs % 100

	return fmt.Sprintf("%s%s.%02d %s", sign, groupThousands(whole), frac, currency)
}

// groupThousands inserts "," every 3 digits from the right, e.g.
// 1234567 -> "1,234,567".
func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
