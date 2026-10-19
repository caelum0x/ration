package tenant

import (
	"context"
	"testing"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
)

type fakeLedger struct {
	createReq *ledgerpb.CreateTenantRequest
	tenant    *ledgerpb.Tenant
	listReq   *ledgerpb.ListTenantsRequest
}

func (f *fakeLedger) CreateTenant(ctx context.Context, req *ledgerpb.CreateTenantRequest) (*ledgerpb.Tenant, error) {
	f.createReq = req
	return &ledgerpb.Tenant{Id: "tenant_new", Name: req.GetName(), Currency: req.GetCurrency(), ReserveFloorMinorUnits: req.GetReserveFloorMinorUnits()}, nil
}

func (f *fakeLedger) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	return f.tenant, nil
}

func (f *fakeLedger) ListTenants(ctx context.Context, req *ledgerpb.ListTenantsRequest) (*ledgerpb.ListTenantsResponse, error) {
	f.listReq = req
	return &ledgerpb.ListTenantsResponse{Tenants: []*ledgerpb.Tenant{f.tenant}}, nil
}

func TestService_Create_Success(t *testing.T) {
	f := &fakeLedger{}
	svc := New(f)

	got, err := svc.Create(context.Background(), "Acme Marketplace", "USD", 20000)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got.GetId() != "tenant_new" {
		t.Errorf("Id = %q, want tenant_new", got.GetId())
	}
	if f.createReq.GetReserveFloorMinorUnits() != 20000 {
		t.Errorf("ReserveFloorMinorUnits sent = %d, want 20000", f.createReq.GetReserveFloorMinorUnits())
	}
}

func TestService_Create_ValidationErrors(t *testing.T) {
	svc := New(&fakeLedger{})

	cases := []struct {
		name, currency string
		reserveFloor   int64
	}{
		{"", "USD", 0},
		{"Acme", "US", 0},
		{"Acme", "USD", -1},
	}
	for _, c := range cases {
		if _, err := svc.Create(context.Background(), c.name, c.currency, c.reserveFloor); err == nil {
			t.Errorf("Create(%q, %q, %d) expected error, got nil", c.name, c.currency, c.reserveFloor)
		}
	}
}

func TestService_Get(t *testing.T) {
	f := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Name: "Acme"}}
	svc := New(f)

	got, err := svc.Get(context.Background(), "tenant_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetName() != "Acme" {
		t.Errorf("Name = %q, want Acme", got.GetName())
	}

	if _, err := svc.Get(context.Background(), ""); err == nil {
		t.Error("Get(\"\") expected error, got nil")
	}
}

func TestService_List(t *testing.T) {
	f := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1"}}
	svc := New(f)

	resp, err := svc.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(resp.GetTenants()) != 1 {
		t.Errorf("len(Tenants) = %d, want 1", len(resp.GetTenants()))
	}
	if f.listReq.GetPageSize() != 10 {
		t.Errorf("PageSize sent = %d, want 10", f.listReq.GetPageSize())
	}
}
