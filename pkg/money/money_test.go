package money

import (
	"errors"
	"math"
	"testing"
)

func TestNew(t *testing.T) {
	m, err := New(1234, "usd")
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if m.MinorUnits() != 1234 {
		t.Errorf("MinorUnits() = %d, want 1234", m.MinorUnits())
	}
	if m.Currency() != "USD" {
		t.Errorf("Currency() = %q, want %q (expected normalization to upper-case)", m.Currency(), "USD")
	}
}

func TestNewNegativeRejected(t *testing.T) {
	_, err := New(-1, "USD")
	if !errors.Is(err, ErrNegative) {
		t.Fatalf("New(-1, ...) error = %v, want ErrNegative", err)
	}
}

func TestZero(t *testing.T) {
	z := Zero("eur")
	if !z.IsZero() {
		t.Errorf("Zero().IsZero() = false, want true")
	}
	if z.Currency() != "EUR" {
		t.Errorf("Zero currency = %q, want EUR", z.Currency())
	}
}

func TestAdd(t *testing.T) {
	a, _ := New(100, "USD")
	b, _ := New(250, "USD")
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if sum.MinorUnits() != 350 {
		t.Errorf("Add() = %d, want 350", sum.MinorUnits())
	}
}

func TestAddCurrencyMismatch(t *testing.T) {
	a, _ := New(100, "USD")
	b, _ := New(100, "EUR")
	_, err := a.Add(b)
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add across currencies error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestAddOverflow(t *testing.T) {
	a, _ := New(math.MaxInt64, "USD")
	b, _ := New(1, "USD")
	_, err := a.Add(b)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("Add overflow error = %v, want ErrOverflow", err)
	}
}

func TestSub(t *testing.T) {
	a, _ := New(500, "USD")
	b, _ := New(200, "USD")
	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub returned error: %v", err)
	}
	if diff.MinorUnits() != 300 {
		t.Errorf("Sub() = %d, want 300", diff.MinorUnits())
	}
}

func TestSubCurrencyMismatch(t *testing.T) {
	a, _ := New(500, "USD")
	b, _ := New(200, "GBP")
	_, err := a.Sub(b)
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub across currencies error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestSubCanGoNegativeWithoutClamping(t *testing.T) {
	a, _ := New(100, "USD")
	b, _ := New(300, "USD")
	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub returned error: %v", err)
	}
	if diff.MinorUnits() != -200 {
		t.Errorf("Sub() = %d, want -200 (Sub must not silently clamp)", diff.MinorUnits())
	}
}

func TestSubUnderflow(t *testing.T) {
	a, _ := New(0, "USD")
	negB := Money{minorUnits: math.MinInt64, currency: "USD"}
	_, err := a.Sub(negB)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("Sub underflow error = %v, want ErrOverflow", err)
	}
}

func TestCmp(t *testing.T) {
	a, _ := New(100, "USD")
	b, _ := New(200, "USD")
	c, _ := New(100, "USD")

	if got, err := a.Cmp(b); err != nil || got != -1 {
		t.Errorf("a.Cmp(b) = (%d, %v), want (-1, nil)", got, err)
	}
	if got, err := b.Cmp(a); err != nil || got != 1 {
		t.Errorf("b.Cmp(a) = (%d, %v), want (1, nil)", got, err)
	}
	if got, err := a.Cmp(c); err != nil || got != 0 {
		t.Errorf("a.Cmp(c) = (%d, %v), want (0, nil)", got, err)
	}
}

func TestCmpCurrencyMismatch(t *testing.T) {
	a, _ := New(100, "USD")
	b, _ := New(100, "JPY")
	if _, err := a.Cmp(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Cmp across currencies error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestFraction(t *testing.T) {
	m, _ := New(1000, "USD")
	got, err := m.Fraction(0.6)
	if err != nil {
		t.Fatalf("Fraction returned error: %v", err)
	}
	if got.MinorUnits() != 600 {
		t.Errorf("Fraction(0.6) = %d, want 600", got.MinorUnits())
	}
}

func TestFractionNegativeRejected(t *testing.T) {
	m, _ := New(1000, "USD")
	if _, err := m.Fraction(-0.1); err == nil {
		t.Fatalf("Fraction(-0.1) returned no error, want an error")
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		minorUnits int64
		currency   string
		want       string
	}{
		{123456, "USD", "1,234.56 USD"},
		{0, "USD", "0.00 USD"},
		{-150, "usd", "-1.50 USD"},
		{1000000, "JPY", "10,000.00 JPY"},
	}
	for _, tc := range cases {
		if got := Format(tc.minorUnits, tc.currency); got != tc.want {
			t.Errorf("Format(%d, %q) = %q, want %q", tc.minorUnits, tc.currency, got, tc.want)
		}
	}
}

func TestSymbol(t *testing.T) {
	if got := Symbol("usd"); got != "$" {
		t.Errorf("Symbol(usd) = %q, want $", got)
	}
	if got := Symbol("XYZ"); got != "" {
		t.Errorf("Symbol(XYZ) = %q, want empty string for unknown currency", got)
	}
}

func TestString(t *testing.T) {
	m, _ := New(123456, "usd")
	if got := m.String(); got != "1,234.56 USD" {
		t.Errorf("String() = %q, want %q", got, "1,234.56 USD")
	}
}

func TestSameCurrency(t *testing.T) {
	a, _ := New(1, "USD")
	b, _ := New(2, "USD")
	c, _ := New(1, "EUR")
	if !a.SameCurrency(b) {
		t.Errorf("SameCurrency(USD, USD) = false, want true")
	}
	if a.SameCurrency(c) {
		t.Errorf("SameCurrency(USD, EUR) = true, want false")
	}
}
