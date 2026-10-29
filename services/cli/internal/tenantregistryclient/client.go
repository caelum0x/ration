// Package tenantregistryclient is a typed gRPC client wrapper around the
// generated pb.TenantRegistryClient, mirroring internal/authclient's
// Dial/Close shape.
//
// This closes a real gap: services/tenant-registry implements tenant
// CRUD (proto/tenant_registry.proto's CreateTenant/GetTenant/
// ListTenants, backed by a real internal/tenant.Service that validates
// name/currency/reserve-floor and delegates to ledger-core) and is
// started by every demo/deploy path (scripts/run-demo.sh,
// scripts/start-individually.sh, docker-compose.yml all bring it up on
// TENANT_REGISTRY_GRPC_ADDR) — but until this package, nothing in this
// repo ever dialed it. `ration seed` creates its 3 fixed demo tenants by
// calling ledger-core's own CreateTenant RPC directly
// (internal/demoseed), bypassing tenant-registry entirely, and no other
// service, the TUI, or the CLI ever called it either. The only way to
// onboard a new platform tenant into a running deployment was to edit
// internal/demoseed.Tenants() in Go source and rebuild — there was no
// operator-facing way to do it at all. `ration tenant create`/`ration
// tenant list` (tenant.go) are tenant-registry's first real callers.
package tenantregistryclient

import (
	"context"
	"fmt"
	"time"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	tenantregistrypb "github.com/caelum0x/ration/pkg/pb/tenantregistry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a TenantRegistryClient gRPC stub with a Go-idiomatic
// method per RPC the CLI needs.
type Client struct {
	conn    *grpc.ClientConn
	stub    tenantregistrypb.TenantRegistryClient
	Timeout time.Duration
}

// Dial connects to tenant-registry at addr. Like the CLI's own
// authclient.Dial, the connection is lazy (gRPC only actually dials on
// first RPC) but addr is still validated as non-empty here so
// misconfiguration fails at startup, not on the first request.
// tenant-registry's own gRPC server installs no inbound auth
// interceptor (see services/tenant-registry/cmd/tenant-registry/main.go
// — it mints its own operator credential for its outgoing ledger-core
// calls, but requires none on its inbound surface, same as audit-log),
// so no credential needs to be attached here.
func Dial(addr string, extraOpts ...grpc.DialOption) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("tenantregistryclient: addr must not be empty")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, extraOpts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("tenantregistryclient: dial %s: %w", addr, err)
	}
	return &Client{
		conn:    conn,
		stub:    tenantregistrypb.NewTenantRegistryClient(conn),
		Timeout: 5 * time.Second,
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CreateTenant onboards a new platform tenant via
// tenant-registry.CreateTenant, which validates name/currency/
// reserve-floor (internal/tenant.Service.Create) and delegates
// persistence to ledger-core.
func (c *Client) CreateTenant(ctx context.Context, name, currency string, reserveFloorMinorUnits int64) (*ledgerpb.Tenant, error) {
	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := c.stub.CreateTenant(rctx, &tenantregistrypb.CreateTenantRequest{
		Name:                   name,
		Currency:               currency,
		ReserveFloorMinorUnits: reserveFloorMinorUnits,
	})
	if err != nil {
		return nil, fmt.Errorf("tenantregistryclient: create tenant: %w", err)
	}
	return resp, nil
}

// ListTenants returns every tenant known to ledger-core via
// tenant-registry.ListTenants.
func (c *Client) ListTenants(ctx context.Context, pageSize int32, pageToken string) (*tenantregistrypb.ListTenantsResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := c.stub.ListTenants(rctx, &tenantregistrypb.ListTenantsRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
	})
	if err != nil {
		return nil, fmt.Errorf("tenantregistryclient: list tenants: %w", err)
	}
	return resp, nil
}
