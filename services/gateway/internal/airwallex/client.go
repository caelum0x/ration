// Package airwallex defines the gateway's abstraction over the Airwallex
// Issuing API, Global Accounts API and webhook delivery. Two
// implementations exist: Live (internal/airwallex live.go), a real HTTP
// client, and Mock (mock.go), a deterministic in-memory fake used when
// AIRWALLEX_MODE=mock (the default). internal/card and internal/bridge
// depend only on the Client interface, never on a concrete
// implementation, so flipping modes is a one-line change in cmd/gateway.
package airwallex

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a card or transaction lookup finds nothing.
var ErrNotFound = errors.New("airwallex: not found")

// CardStatus mirrors the subset of Airwallex issuing-card statuses the
// gateway cares about.
type CardStatus string

const (
	CardStatusActive    CardStatus = "ACTIVE"
	CardStatusSuspended CardStatus = "SUSPENDED"
	CardStatusClosed    CardStatus = "CLOSED"
)

// Card is the Airwallex-side representation of an issued card. It is
// intentionally distinct from pb.Card (the gateway's own wire type) so the
// mapping between "what Airwallex thinks" and "what we expose over gRPC"
// stays explicit at the internal/card boundary.
type Card struct {
	ID              string
	CardholderName  string
	Status          CardStatus
	LimitMinorUnits int64
	LimitInterval   string // "daily" | "monthly"
	Currency        string
	CreatedAt       time.Time
}

// IssueCardParams are the inputs needed to create a new issuing card.
type IssueCardParams struct {
	TenantID        string
	CardholderName  string
	LimitMinorUnits int64
	LimitInterval   string
	Currency        string
}

// CardControls restricts how a card can be spent — used both at issuance
// and when the agent tightens controls alongside a partial bridge grant.
type CardControls struct {
	LimitMinorUnits             int64
	LimitInterval               string
	AllowedCategories           []string // empty = unrestricted
	PerTransactionCapMinorUnits int64    // 0 = unrestricted
}

// Transaction is a Global Account transaction (e.g. an incoming deposit).
type Transaction struct {
	ID               string
	AmountMinorUnits int64
	Currency         string
	Description      string
	OccurredAt       time.Time
}

// ListGlobalAccountTransactionsParams scopes a transaction listing.
type ListGlobalAccountTransactionsParams struct {
	TenantID string
	Since    time.Time
}

// Client is the interface the rest of the gateway depends on. Both Live
// and Mock implement it identically so swapping AIRWALLEX_MODE never
// requires touching internal/card, internal/bridge or internal/grpcserver.
type Client interface {
	IssueCard(ctx context.Context, params IssueCardParams) (*Card, error)
	GetCard(ctx context.Context, cardID string) (*Card, error)
	SetCardControls(ctx context.Context, cardID string, controls CardControls) (*Card, error)
	ListGlobalAccountTransactions(ctx context.Context, params ListGlobalAccountTransactionsParams) ([]Transaction, error)
	VerifyWebhookSignature(payload []byte, timestamp string, signature string) error
}
