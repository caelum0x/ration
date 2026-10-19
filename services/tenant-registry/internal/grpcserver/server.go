// Package grpcserver implements tenant_registry.proto's TenantRegistry
// service by delegating to internal/tenant — this package is the one
// place that translates between wire types (pkg/pb/tenantregistry,
// pkg/pb/ledger) and internal/tenant's Go-native API.
package grpcserver

import (
	"context"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	tenantregistrypb "github.com/caelum0x/ration/pkg/pb/tenantregistry"
	"github.com/caelum0x/ration/services/tenant-registry/internal/tenant"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements tenantregistrypb.TenantRegistryServer.
type Server struct {
	tenantregistrypb.UnimplementedTenantRegistryServer

	svc *tenant.Service
}

// New wraps a *tenant.Service as a TenantRegistryServer.
func New(svc *tenant.Service) *Server {
	return &Server{svc: svc}
}

// CreateTenant implements tenantregistrypb.TenantRegistryServer.
func (s *Server) CreateTenant(ctx context.Context, req *tenantregistrypb.CreateTenantRequest) (*ledgerpb.Tenant, error) {
	t, err := s.svc.Create(ctx, req.GetName(), req.GetCurrency(), req.GetReserveFloorMinorUnits())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return t, nil
}

// GetTenant implements tenantregistrypb.TenantRegistryServer.
func (s *Server) GetTenant(ctx context.Context, req *tenantregistrypb.GetTenantRequest) (*ledgerpb.Tenant, error) {
	t, err := s.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return t, nil
}

// ListTenants implements tenantregistrypb.TenantRegistryServer.
func (s *Server) ListTenants(ctx context.Context, req *tenantregistrypb.ListTenantsRequest) (*tenantregistrypb.ListTenantsResponse, error) {
	resp, err := s.svc.List(ctx, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantregistrypb.ListTenantsResponse{
		Tenants:       resp.GetTenants(),
		NextPageToken: resp.GetNextPageToken(),
	}, nil
}
