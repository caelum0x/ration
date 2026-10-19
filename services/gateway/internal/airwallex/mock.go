package airwallex

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Mock is a deterministic in-memory fake of the Airwallex Issuing +
// Global Accounts API, used when AIRWALLEX_MODE=mock (the default). It
// mirrors the Client interface byte-for-byte with Live so flipping to
// AIRWALLEX_MODE=live with real sandbox credentials is a one-line env
// change, not a rewrite.
//
// Determinism: card IDs are derived from a monotonic counter rather than
// random bytes, and Now is injectable, so demo output and tests are
// reproducible run to run.
type Mock struct {
	mu     sync.Mutex
	cards  map[string]*Card
	nextID int
	Now    func() time.Time
	Secret string // webhook secret used for VerifyWebhookSignature in mock mode

	// txByTenant lets the CLI/demo driver seed fake incoming deposits that
	// ListGlobalAccountTransactions will then return, mirroring how a real
	// Airwallex global account would surface a received deposit.
	txByTenant map[string][]Transaction
}

// NewMock constructs a ready-to-use Mock client.
func NewMock(webhookSecret string) *Mock {
	return &Mock{
		cards:      make(map[string]*Card),
		txByTenant: make(map[string][]Transaction),
		Now:        time.Now,
		Secret:     webhookSecret,
	}
}

func (m *Mock) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// IssueCard creates a deterministic fake card and stores it in memory.
func (m *Mock) IssueCard(ctx context.Context, params IssueCardParams) (*Card, error) {
	if params.CardholderName == "" {
		return nil, fmt.Errorf("airwallex mock: cardholder_name is required")
	}
	if params.LimitMinorUnits < 0 {
		return nil, fmt.Errorf("airwallex mock: limit_minor_units must be >= 0")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextID++
	card := &Card{
		ID:              fmt.Sprintf("aw_card_mock_%06d", m.nextID),
		CardholderName:  params.CardholderName,
		Status:          CardStatusActive,
		LimitMinorUnits: params.LimitMinorUnits,
		LimitInterval:   params.LimitInterval,
		Currency:        params.Currency,
		CreatedAt:       m.now(),
	}
	m.cards[card.ID] = card
	return cloneCard(card), nil
}

// GetCard returns the previously issued fake card.
func (m *Mock) GetCard(ctx context.Context, cardID string) (*Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	card, ok := m.cards[cardID]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneCard(card), nil
}

// SetCardControls updates the fake card's limit/controls in place.
func (m *Mock) SetCardControls(ctx context.Context, cardID string, controls CardControls) (*Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	card, ok := m.cards[cardID]
	if !ok {
		return nil, ErrNotFound
	}
	if controls.LimitMinorUnits < 0 {
		return nil, fmt.Errorf("airwallex mock: limit_minor_units must be >= 0")
	}
	card.LimitMinorUnits = controls.LimitMinorUnits
	if controls.LimitInterval != "" {
		card.LimitInterval = controls.LimitInterval
	}
	return cloneCard(card), nil
}

// SeedTransaction lets the demo/CLI layer inject a fake incoming global
// account transaction for a tenant, so ListGlobalAccountTransactions has
// something deterministic to return without a real Airwallex sandbox.
func (m *Mock) SeedTransaction(tenantID string, tx Transaction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txByTenant[tenantID] = append(m.txByTenant[tenantID], tx)
}

// ListGlobalAccountTransactions returns seeded fake transactions for a
// tenant, filtered by Since and sorted oldest-first.
func (m *Mock) ListGlobalAccountTransactions(ctx context.Context, params ListGlobalAccountTransactionsParams) ([]Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.txByTenant[params.TenantID]
	out := make([]Transaction, 0, len(all))
	for _, tx := range all {
		if !params.Since.IsZero() && tx.OccurredAt.Before(params.Since) {
			continue
		}
		out = append(out, tx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

// VerifyWebhookSignature checks a mock-mode-simulated webhook delivery
// using the same HMAC scheme as the live client, so the signature
// verification code path is exercised identically in demo mode.
func (m *Mock) VerifyWebhookSignature(payload []byte, timestamp string, signature string) error {
	return verifySignature(m.Secret, payload, timestamp, signature)
}

func cloneCard(c *Card) *Card {
	cp := *c
	return &cp
}

var _ Client = (*Mock)(nil)
