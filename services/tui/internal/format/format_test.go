package format

import (
	"testing"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
)

func TestMoney(t *testing.T) {
	cases := []struct {
		name       string
		minorUnits int64
		currency   string
		want       string
	}{
		{"usd simple", 1234, "USD", "$12.34"},
		{"usd grouped thousands", 123456789, "USD", "$1,234,567.89"},
		{"zero", 0, "USD", "$0.00"},
		{"negative", -500, "USD", "-$5.00"},
		{"eur symbol", 100, "EUR", "€1.00"},
		{"unknown currency falls back to code", 100, "XYZ", "XYZ 1.00"},
		{"lowercase currency normalizes", 100, "usd", "$1.00"},
		{"single digit minor", 1005, "USD", "$10.05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Money(tc.minorUnits, tc.currency)
			if got != tc.want {
				t.Errorf("Money(%d, %q) = %q, want %q", tc.minorUnits, tc.currency, got, tc.want)
			}
		})
	}
}

func TestGroupThousands(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
	}
	for _, tc := range cases {
		got := groupThousands(tc.n)
		if got != tc.want {
			t.Errorf("groupThousands(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestUnix(t *testing.T) {
	if got := Unix(0); got != "-" {
		t.Errorf("Unix(0) = %q, want %q", got, "-")
	}
	// 2021-01-01 00:00:00 UTC
	if got, want := Unix(1609459200), "2021-01-01 00:00:00"; got != want {
		t.Errorf("Unix(1609459200) = %q, want %q", got, want)
	}
}

func TestCardStatusLabel(t *testing.T) {
	cases := []struct {
		s    commonpb.CardStatus
		want string
	}{
		{commonpb.CardStatus_CARD_STATUS_ACTIVE, "ACTIVE"},
		{commonpb.CardStatus_CARD_STATUS_RATIONED, "RATIONED"},
		{commonpb.CardStatus_CARD_STATUS_SUSPENDED, "SUSPENDED"},
		{commonpb.CardStatus_CARD_STATUS_CLOSED, "CLOSED"},
		{commonpb.CardStatus_CARD_STATUS_UNSPECIFIED, "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := CardStatusLabel(tc.s); got != tc.want {
			t.Errorf("CardStatusLabel(%v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestDepositStatusLabel(t *testing.T) {
	cases := []struct {
		s    commonpb.DepositStatus
		want string
	}{
		{commonpb.DepositStatus_DEPOSIT_STATUS_EXPECTED, "EXPECTED"},
		{commonpb.DepositStatus_DEPOSIT_STATUS_RECEIVED, "RECEIVED"},
		{commonpb.DepositStatus_DEPOSIT_STATUS_LATE, "LATE"},
		{commonpb.DepositStatus_DEPOSIT_STATUS_UNSPECIFIED, "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := DepositStatusLabel(tc.s); got != tc.want {
			t.Errorf("DepositStatusLabel(%v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello w…"},
		{"hello", 1, "h"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"héllo world", 8, "héllo w…"},
	}
	for _, tc := range cases {
		got := Truncate(tc.s, tc.width)
		if got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
		}
	}
}
