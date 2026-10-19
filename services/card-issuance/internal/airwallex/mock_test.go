package airwallex

import (
	"context"
	"testing"
	"time"
)

func TestMock_IssueCardAndGetCard(t *testing.T) {
	m := NewMock()
	m.Now = func() time.Time { return time.Unix(1700000000, 0) }

	card, err := m.IssueCard(context.Background(), IssueCardParams{
		TenantID:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 10000,
		LimitInterval:   "daily",
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}
	if card.ID == "" {
		t.Fatal("IssueCard() returned empty card ID")
	}
	if card.Status != CardStatusActive {
		t.Errorf("Status = %v, want %v", card.Status, CardStatusActive)
	}

	got, err := m.GetCard(context.Background(), card.ID)
	if err != nil {
		t.Fatalf("GetCard() error = %v", err)
	}
	if got.CardholderName != "Ada Lovelace" {
		t.Errorf("CardholderName = %q, want %q", got.CardholderName, "Ada Lovelace")
	}
}

func TestMock_GetCard_NotFound(t *testing.T) {
	m := NewMock()
	_, err := m.GetCard(context.Background(), "does-not-exist")
	if err != ErrNotFound {
		t.Fatalf("GetCard() error = %v, want %v", err, ErrNotFound)
	}
}

func TestMock_SetCardControls(t *testing.T) {
	m := NewMock()
	card, err := m.IssueCard(context.Background(), IssueCardParams{
		CardholderName:  "Grace Hopper",
		LimitMinorUnits: 5000,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	updated, err := m.SetCardControls(context.Background(), card.ID, CardControls{
		LimitMinorUnits: 2000,
	})
	if err != nil {
		t.Fatalf("SetCardControls() error = %v", err)
	}
	if updated.LimitMinorUnits != 2000 {
		t.Errorf("LimitMinorUnits = %d, want 2000", updated.LimitMinorUnits)
	}
}

func TestMock_IssueCard_RequiresCardholderName(t *testing.T) {
	m := NewMock()
	if _, err := m.IssueCard(context.Background(), IssueCardParams{}); err == nil {
		t.Fatal("IssueCard() expected error for missing cardholder name, got nil")
	}
}

func TestMock_ListGlobalAccountTransactions(t *testing.T) {
	m := NewMock()
	early := time.Unix(1000, 0)
	late := time.Unix(2000, 0)
	m.SeedTransaction("tenant_1", Transaction{ID: "tx_late", AmountMinorUnits: 500, OccurredAt: late})
	m.SeedTransaction("tenant_1", Transaction{ID: "tx_early", AmountMinorUnits: 100, OccurredAt: early})

	txs, err := m.ListGlobalAccountTransactions(context.Background(), ListGlobalAccountTransactionsParams{TenantID: "tenant_1"})
	if err != nil {
		t.Fatalf("ListGlobalAccountTransactions() error = %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("len(txs) = %d, want 2", len(txs))
	}
	if txs[0].ID != "tx_early" {
		t.Errorf("txs[0].ID = %q, want tx_early (sorted oldest-first)", txs[0].ID)
	}

	filtered, err := m.ListGlobalAccountTransactions(context.Background(), ListGlobalAccountTransactionsParams{
		TenantID: "tenant_1",
		Since:    time.Unix(1500, 0),
	})
	if err != nil {
		t.Fatalf("ListGlobalAccountTransactions() error = %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "tx_late" {
		t.Fatalf("filtered = %+v, want only tx_late", filtered)
	}
}
