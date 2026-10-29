// Package airwallex defines card-issuance's abstraction over the
// Airwallex Issuing API and Global Accounts API. Two implementations
// exist: Live (live.go), a real HTTP client, and Mock (mock.go), a
// deterministic in-memory fake used when AIRWALLEX_MODE=mock (the
// default). internal/cardsvc and internal/bridge depend only on the
// Client interface, never on a concrete implementation, so flipping
// modes is a one-line change in cmd/card-issuance.
//
// This is card-issuance's own copy of the gateway's former
// internal/airwallex package (docs/adr/0004): webhook signature
// verification moved to services/webhook-ingestion instead, since
// card-issuance never receives webhook deliveries, so this copy has no
// VerifyWebhookSignature method and no webhook secret to thread through.
package airwallex

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a card or transaction lookup finds nothing.
var ErrNotFound = errors.New("airwallex: not found")

// CardStatus mirrors the subset of Airwallex issuing-card statuses
// card-issuance cares about.
type CardStatus string

const (
	CardStatusActive    CardStatus = "ACTIVE"
	CardStatusSuspended CardStatus = "SUSPENDED"
	CardStatusClosed    CardStatus = "CLOSED"
)

// Card is the Airwallex-side representation of an issued card. It is
// intentionally distinct from pb.Card (card-issuance's own wire type) so
// the mapping between "what Airwallex thinks" and "what we expose over
// gRPC" stays explicit at the internal/cardsvc boundary.
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

// Client is the interface the rest of card-issuance depends on. Both
// Live and Mock implement it identically so swapping AIRWALLEX_MODE
// never requires touching internal/cardsvc, internal/bridge or
// internal/grpcserver.
type Client interface {
	IssueCard(ctx context.Context, params IssueCardParams) (*Card, error)
	GetCard(ctx context.Context, cardID string) (*Card, error)
	SetCardControls(ctx context.Context, cardID string, controls CardControls) (*Card, error)
	// CloseCard permanently closes a card (CardStatusClosed) — the
	// Airwallex-side counterpart to this package's own CardStatusClosed
	// constant, which existed since the original build but, until this
	// method, nothing ever called: SetCardControls can only change a
	// card's limit/controls, never its status.
	CloseCard(ctx context.Context, cardID string) (*Card, error)
	ListGlobalAccountTransactions(ctx context.Context, params ListGlobalAccountTransactionsParams) ([]Transaction, error)
}
