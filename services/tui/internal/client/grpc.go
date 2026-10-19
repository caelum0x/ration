// Package client wires up the TUI's gRPC connections to the gateway and
// ledger-core services and exposes the handful of read paths the dashboard
// needs (tenants, deposits, cards, the live activity stream).
package client

import (
	"context"
	"fmt"
	"time"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialTimeout bounds how long Dial waits for the initial connection
// handshake before giving up.
const DialTimeout = 5 * time.Second

// Clients bundles the gateway and ledger-core gRPC clients the TUI talks
// to. Both are plain generated stubs from pkg/pb — this type just owns the
// connections and exposes typed convenience methods over them.
type Clients struct {
	Gateway gatewaypb.GatewayClient
	Ledger  ledgerpb.LedgerCoreClient

	gatewayConn *grpc.ClientConn
	ledgerConn  *grpc.ClientConn
}

// Dial connects to the gateway and ledger-core gRPC servers at the given
// addresses. Connections are plaintext (insecure transport credentials) —
// matching a local/demo deployment behind a trusted network, consistent
// with the rest of this project's mock-first Airwallex posture.
func Dial(gatewayAddr, ledgerAddr string) (*Clients, error) {
	gwConn, err := grpc.NewClient(gatewayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial gateway at %s: %w", gatewayAddr, err)
	}

	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		gwConn.Close()
		return nil, fmt.Errorf("dial ledger-core at %s: %w", ledgerAddr, err)
	}

	return &Clients{
		Gateway:     gatewaypb.NewGatewayClient(gwConn),
		Ledger:      ledgerpb.NewLedgerCoreClient(ledgerConn),
		gatewayConn: gwConn,
		ledgerConn:  ledgerConn,
	}, nil
}

// Close tears down both underlying connections. Errors are joined so
// callers see both failures if both occur.
func (c *Clients) Close() error {
	var errs []error
	if err := c.gatewayConn.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close gateway conn: %w", err))
	}
	if err := c.ledgerConn.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close ledger conn: %w", err))
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("%v; %v", errs[0], errs[1])
	}
}

// ListTenants fetches every tenant from ledger-core. Pagination is not yet
// exercised by the dashboard (demo scale is a handful of tenants) but the
// request still passes a generous page size rather than relying on an
// unspecified server default.
func (c *Clients) ListTenants(ctx context.Context) ([]*ledgerpb.Tenant, error) {
	resp, err := c.Ledger.ListTenants(ctx, &ledgerpb.ListTenantsRequest{PageSize: 500})
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return resp.GetTenants(), nil
}

// ListDeposits fetches every deposit for the given tenant from ledger-core.
func (c *Clients) ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error) {
	resp, err := c.Ledger.ListDeposits(ctx, &ledgerpb.ListDepositsRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("list deposits for tenant %s: %w", tenantID, err)
	}
	return resp.GetDeposits(), nil
}

// ListCards fetches every card for the given tenant from the gateway.
func (c *Clients) ListCards(ctx context.Context, tenantID string) ([]*gatewaypb.Card, error) {
	resp, err := c.Gateway.ListCards(ctx, &gatewaypb.ListCardsRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("list cards for tenant %s: %w", tenantID, err)
	}
	return resp.GetCards(), nil
}

// GetBalance fetches the current ledger balance for a tenant.
func (c *Clients) GetBalance(ctx context.Context, tenantID string) (*ledgerpb.Balance, error) {
	bal, err := c.Ledger.GetBalance(ctx, &ledgerpb.GetBalanceRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("get balance for tenant %s: %w", tenantID, err)
	}
	return bal, nil
}

// StreamActivity opens the gateway's live activity feed for all tenants
// (an empty tenant_id, per gateway.proto, means "all tenants"). The caller
// owns the returned stream's lifecycle and should Recv() in a loop until
// ctx is canceled or an error is returned.
func (c *Clients) StreamActivity(ctx context.Context) (gatewaypb.Gateway_StreamActivityClient, error) {
	stream, err := c.Gateway.StreamActivity(ctx, &gatewaypb.StreamActivityRequest{})
	if err != nil {
		return nil, fmt.Errorf("stream activity: %w", err)
	}
	return stream, nil
}
