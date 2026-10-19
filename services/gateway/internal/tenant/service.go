// Package tenant provides tenant CRUD for the gateway. It owns no state
// of its own — every operation delegates persistence to ledger-core,
// which is the authoritative source of truth for tenant records per
// docs/ARCHITECTURE.md.
package tenant

import (
	"context"
	"fmt"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
)

// LedgerClient is the subset of ledgerclient.Client this package needs.
type LedgerClient interface {
	CreateTenant(ctx context.Context, req *ledgerpb.CreateTenantRequest) (*ledgerpb.Tenant, error)
	GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error)
	ListTenants(ctx context.Context, req *ledgerpb.ListTenantsRequest) (*ledgerpb.ListTenantsResponse, error)
}

// Service implements tenant CRUD on top of a LedgerClient.
type Service struct {
	ledger LedgerClient
}

// New constructs a tenant Service.
func New(ledger LedgerClient) *Service {
	return &Service{ledger: ledger}
}

// Create validates and creates a new tenant.
func (s *Service) Create(ctx context.Context, name, currency string, reserveFloorMinorUnits int64) (*ledgerpb.Tenant, error) {
	if name == "" {
		return nil, fmt.Errorf("tenant: name is required")
	}
	if len(currency) != 3 {
		return nil, fmt.Errorf("tenant: currency must be a 3-letter ISO 4217 code, got %q", currency)
	}
	if reserveFloorMinorUnits < 0 {
		return nil, fmt.Errorf("tenant: reserve_floor_minor_units must be >= 0, got %d", reserveFloorMinorUnits)
	}
	return s.ledger.CreateTenant(ctx, &ledgerpb.CreateTenantRequest{
		Name:                   name,
		Currency:               currency,
		ReserveFloorMinorUnits: reserveFloorMinorUnits,
	})
}

// Get fetches a tenant by ID.
func (s *Service) Get(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenant: id is required")
	}
	return s.ledger.GetTenant(ctx, tenantID)
}

// List returns a page of tenants.
func (s *Service) List(ctx context.Context, pageSize int32, pageToken string) (*ledgerpb.ListTenantsResponse, error) {
	return s.ledger.ListTenants(ctx, &ledgerpb.ListTenantsRequest{PageSize: pageSize, PageToken: pageToken})
}
