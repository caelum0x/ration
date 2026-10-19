package moneyfmt

import "testing"

func TestMinor(t *testing.T) {
	cases := []struct {
		name       string
		minorUnits int64
		currency   string
		want       string
	}{
		{"zero", 0, "USD", "0.00 USD"},
		{"under a dollar", 7, "USD", "0.07 USD"},
		{"exact dollars", 250_000, "USD", "2,500.00 USD"},
		{"large with multiple commas", 15_000_000, "GBP", "150,000.00 GBP"},
		{"six-digit no leading group", 123_456_00, "EUR", "123,456.00 EUR"},
		{"negative", -250_000, "USD", "-2,500.00 USD"},
		{"single cent negative", -1, "USD", "-0.01 USD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Minor(tc.minorUnits, tc.currency)
			if got != tc.want {
				t.Errorf("Minor(%d, %q) = %q, want %q", tc.minorUnits, tc.currency, got, tc.want)
			}
		})
	}
}
