// Package money is the Go mirror of the Money type described in
// docs/ARCHITECTURE.md's "Money type" section: fixed-point integer minor
// units (int64, e.g. cents) + an ISO 4217 currency code. Never float.
//
// This type is the shared business-logic representation imported by
// gateway/agent/tui/cli wherever Money values are added, subtracted, or
// compared rather than merely passed through as *common.Money protobuf
// structs. Arithmetic is checked: overflow and cross-currency operations
// return a typed error instead of silently producing a wrong balance,
// mirroring ledger-core's Rust Money type (checked add/sub, currency
// mismatches are a typed error, not a panic).
package money

import (
	"errors"
	"fmt"
	"strings"
)

// ErrCurrencyMismatch is returned when an arithmetic operation is
// attempted between two Money values with different currencies.
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// ErrOverflow is returned when an arithmetic operation would overflow
// (or underflow) the int64 minor-units representation.
var ErrOverflow = errors.New("money: overflow")

// ErrNegative is returned by operations that require a non-negative
// amount (e.g. New) when given a negative minor-units value.
var ErrNegative = errors.New("money: negative amount not allowed")

// Money is a fixed-point amount in minor units (e.g. cents) of a single
// ISO 4217 currency. The zero value is 0 in an empty currency and is not
// generally useful on its own; construct values with New or Zero.
type Money struct {
	minorUnits int64
	currency   string
}

// New constructs a Money value. currency is upper-cased for consistent
// comparisons (mirrors Rust Money's currency normalization). Returns
// ErrNegative if minorUnits is negative, since every current caller
// (balances, grants, deposits) only ever represents non-negative amounts;
// callers that need signed deltas should track sign separately.
func New(minorUnits int64, currency string) (Money, error) {
	if minorUnits < 0 {
		return Money{}, fmt.Errorf("money: New(%d, %q): %w", minorUnits, currency, ErrNegative)
	}
	return Money{minorUnits: minorUnits, currency: normalizeCurrency(currency)}, nil
}

// Zero returns a zero-value Money in the given currency.
func Zero(currency string) Money {
	return Money{minorUnits: 0, currency: normalizeCurrency(currency)}
}

// MinorUnits returns the underlying fixed-point amount.
func (m Money) MinorUnits() int64 { return m.minorUnits }

// Currency returns the ISO 4217 currency code (upper-cased).
func (m Money) Currency() string { return m.currency }

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.minorUnits == 0 }

// SameCurrency reports whether m and other share a currency.
func (m Money) SameCurrency(other Money) bool {
	return m.currency == other.currency
}

// Add returns m + other. Returns ErrCurrencyMismatch if the currencies
// differ, or ErrOverflow if the sum would overflow int64.
func (m Money) Add(other Money) (Money, error) {
	if !m.SameCurrency(other) {
		return Money{}, fmt.Errorf("money: Add(%s, %s): %w", m.currency, other.currency, ErrCurrencyMismatch)
	}
	sum, ok := addInt64(m.minorUnits, other.minorUnits)
	if !ok {
		return Money{}, fmt.Errorf("money: Add(%d, %d): %w", m.minorUnits, other.minorUnits, ErrOverflow)
	}
	return Money{minorUnits: sum, currency: m.currency}, nil
}

// Sub returns m - other. Returns ErrCurrencyMismatch if the currencies
// differ, or ErrOverflow if the difference would overflow/underflow
// int64. Sub does NOT clamp at zero — callers that need a non-negative
// result (e.g. headroom math) should clamp explicitly; this keeps the
// type honest about what actually happened, matching ledger-core's
// checked arithmetic rather than silently losing information.
func (m Money) Sub(other Money) (Money, error) {
	if !m.SameCurrency(other) {
		return Money{}, fmt.Errorf("money: Sub(%s, %s): %w", m.currency, other.currency, ErrCurrencyMismatch)
	}
	diff, ok := subInt64(m.minorUnits, other.minorUnits)
	if !ok {
		return Money{}, fmt.Errorf("money: Sub(%d, %d): %w", m.minorUnits, other.minorUnits, ErrOverflow)
	}
	return Money{minorUnits: diff, currency: m.currency}, nil
}

// Cmp compares m and other, returning -1, 0, or 1 as m is less than,
// equal to, or greater than other. Returns ErrCurrencyMismatch if the
// currencies differ, since cross-currency amounts are not orderable
// without an exchange rate this package deliberately does not own.
func (m Money) Cmp(other Money) (int, error) {
	if !m.SameCurrency(other) {
		return 0, fmt.Errorf("money: Cmp(%s, %s): %w", m.currency, other.currency, ErrCurrencyMismatch)
	}
	switch {
	case m.minorUnits < other.minorUnits:
		return -1, nil
	case m.minorUnits > other.minorUnits:
		return 1, nil
	default:
		return 0, nil
	}
}

// Fraction returns a new Money equal to m scaled by frac (e.g. 0.6 for
// the 60% bridge cap in docs/AGENT-DESIGN.md). frac must be >= 0; the
// result is truncated toward zero, matching the existing bridge-cap
// behavior (int64(float64(x) * frac)). Returns an error if frac < 0.
func (m Money) Fraction(frac float64) (Money, error) {
	if frac < 0 {
		return Money{}, fmt.Errorf("money: Fraction(%g): negative fraction not allowed", frac)
	}
	scaled := int64(float64(m.minorUnits) * frac)
	return Money{minorUnits: scaled, currency: m.currency}, nil
}

// String renders m as "1,234.56 USD" (see also the package-level Format
// function for symbol-prefixed rendering used by the TUI).
func (m Money) String() string {
	return Format(m.minorUnits, m.currency)
}

// addInt64 adds a and b, reporting ok=false on overflow.
func addInt64(a, b int64) (int64, bool) {
	sum := a + b
	// Overflow occurred iff the operands have the same sign and the
	// result's sign differs from theirs.
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// subInt64 subtracts b from a, reporting ok=false on overflow/underflow.
func subInt64(a, b int64) (int64, bool) {
	diff := a - b
	if (b < 0 && diff < a) || (b > 0 && diff > a) {
		return 0, false
	}
	return diff, true
}

func normalizeCurrency(currency string) string {
	return strings.ToUpper(strings.TrimSpace(currency))
}

var currencySymbols = map[string]string{
	"USD": "$",
	"EUR": "€",
	"GBP": "£",
	"JPY": "¥",
}

// Format renders a fixed-point minor-units amount with its ISO 4217
// currency as "1,234.56 USD" (unknown currencies keep the "CODE amount"
// shape rather than guessing a symbol). This is the shared formatting
// helper referenced by docs/ARCHITECTURE.md's Cross-cutting section;
// service-local formatters (e.g. the TUI's symbol-prefixed "$1,234.56")
// may still wrap it for presentation-specific needs.
func Format(minorUnits int64, currency string) string {
	sign := ""
	abs := minorUnits
	if abs < 0 {
		sign = "-"
		abs = -abs
	}

	whole := abs / 100
	frac := abs % 100

	return fmt.Sprintf("%s%s.%02d %s", sign, groupThousands(whole), frac, normalizeCurrency(currency))
}

// Symbol returns the currency symbol for well-known ISO 4217 codes
// ("USD" -> "$"), or "" if unknown.
func Symbol(currency string) string {
	return currencySymbols[normalizeCurrency(currency)]
}

// groupThousands inserts "," every three digits from the right, e.g.
// groupThousands(1234567) -> "1,234,567". n must be >= 0.
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
