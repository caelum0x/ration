// Package client wires up the TUI's gRPC connections to the gateway and
// ledger-core services and exposes the handful of read paths the dashboard
// needs (tenants, deposits, cards, the live activity stream).
package client

import (
	"context"
	"fmt"
	"time"

	"github.com/caelum0x/ration/pkg/mtls"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/tui/internal/authclient"
	"google.golang.org/grpc"
)

// DialTimeout bounds how long Dial waits for the initial connection
// handshake before giving up.
const DialTimeout = 5 * time.Second

// operatorCredentialTTL bounds how long the TUI's one minted operator
// JWT (see internal/authclient.MintOperatorJWT) stays valid. The TUI is
// a long-running dashboard (unlike a one-shot CLI invocation), so this
// is deliberately close to jwtauth.MaxTTL (24h) rather than a short
// default — a dashboard session dying mid-demo because its credential
// silently expired would be a worse failure mode than a slightly
// longer-lived token.
const operatorCredentialTTL = 20 * time.Hour

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
// addresses, minting a single operator-scoped JWT from auth at authAddr
// first (see internal/authclient.MintOperatorJWT) and attaching it to
// every subsequent gateway AND ledger-core RPC — closing
// docs/THREAT-MODEL.md's "Known gaps" item 1's own named residual TODO
// for the TUI's half of it, including "the biggest one" sub-gap:
// ledger-core now verifies this same credential itself
// (services/ledger-core/src/tenantauth.rs), rather than trusting
// whatever already passed gateway's internal/tenantauth.
//
// Plaintext by default (matching a local/demo deployment behind a
// trusted network, consistent with the rest of this project's
// mock-first Airwallex posture) — set TUI_MTLS_ENABLED=true (see
// docs/SCALE-EXPANSION.md section 8, pkg/mtls, infra/dev-ca/) to
// require mTLS against both peers instead.
func Dial(ctx context.Context, gatewayAddr, ledgerAddr, authAddr string) (*Clients, error) {
	// Every RPC the TUI issues carries a trace (otelgrpc stats handler)
	// and a correlation ID (minted fresh per RPC since the TUI has no
	// single "command" root the way cli does) so a judge or operator can
	// follow a dashboard refresh all the way into ledger-core.
	commonDialOpts := []grpc.DialOption{
		telemetry.DialOption(),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(telemetry.StreamClientInterceptor()),
	}

	// The TUI dials two different peers with its one identity, so the
	// server name to verify each peer's certificate against is set per
	// dial (see mtls.Config.WithDefaultServerName) rather than once.
	mtlsCfg, err := mtls.LoadFromEnv("TUI")
	if err != nil {
		return nil, fmt.Errorf("load mTLS config: %w", err)
	}

	// This dial must present the TUI's own TUI_MTLS_* identity to auth
	// too, same as the gateway/ledger-core dials below — auth's server
	// requires and verifies a client cert the moment AUTH_MTLS_ENABLED
	// is true, and a plaintext dial against that listener fails the
	// TLS handshake outright, not a graceful degrade (confirmed live
	// against a real mTLS-enabled auth instance). Mirrors gateway's
	// own authMTLS/authDialCreds pattern
	// (services/gateway/cmd/gateway/main.go).
	authDialOpt, err := mtlsCfg.WithDefaultServerName("auth").DialOption()
	if err != nil {
		return nil, fmt.Errorf("mTLS dial option for auth: %w", err)
	}
	auth, err := authclient.Dial(authAddr, authDialOpt)
	if err != nil {
		return nil, fmt.Errorf("dial auth at %s: %w", authAddr, err)
	}
	defer auth.Close()
	token, _, err := auth.MintOperatorJWT(ctx, "ration-tui", operatorCredentialTTL)
	if err != nil {
		return nil, fmt.Errorf("mint operator credential from auth at %s: %w", authAddr, err)
	}
	gatewayDialOpts := append([]grpc.DialOption{
		grpc.WithChainUnaryInterceptor(authclient.UnaryClientInterceptor(token)),
		grpc.WithChainStreamInterceptor(authclient.StreamClientInterceptor(token)),
	}, commonDialOpts...)

	gatewayDialOpt, err := mtlsCfg.WithDefaultServerName("gateway").DialOption()
	if err != nil {
		return nil, fmt.Errorf("mTLS dial option for gateway: %w", err)
	}
	gwConn, err := grpc.NewClient(gatewayAddr, append([]grpc.DialOption{gatewayDialOpt}, gatewayDialOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("dial gateway at %s: %w", gatewayAddr, err)
	}

	ledgerDialOpt, err := mtlsCfg.WithDefaultServerName("ledger-core").DialOption()
	if err != nil {
		gwConn.Close()
		return nil, fmt.Errorf("mTLS dial option for ledger-core: %w", err)
	}
	ledgerDialOpts := append([]grpc.DialOption{
		ledgerDialOpt,
		grpc.WithChainUnaryInterceptor(authclient.UnaryClientInterceptor(token)),
		grpc.WithChainStreamInterceptor(authclient.StreamClientInterceptor(token)),
	}, commonDialOpts...)
	ledgerConn, err := grpc.NewClient(ledgerAddr, ledgerDialOpts...)
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

// ListAgentDecisions fetches a tenant's most recent agent decisions
// (newest first, bounded by limit) from ledger-core — the data behind
// the Decisions tab (internal/view.Decisions): every rationale,
// policy_snapshot_json and chosen action the agent's loop has recorded
// for this tenant, not just the balance/card state its decisions acted
// on.
func (c *Clients) ListAgentDecisions(ctx context.Context, tenantID string, limit int32) ([]*ledgerpb.AgentDecision, error) {
	resp, err := c.Ledger.ListAgentDecisions(ctx, &ledgerpb.ListAgentDecisionsRequest{TenantId: tenantID, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list agent decisions for tenant %s: %w", tenantID, err)
	}
	return resp.GetDecisions(), nil
}

// IssueCard issues a new card for a tenant via the gateway, the same
// gateway.IssueCard RPC services/cli's "ration card issue" calls — the
// TUI's Cards tab had no caller of this RPC at all before this (see
// model.newIssueCardForm): it could only ever display cards the
// agent's own RationBridgeFunding side effects created.
func (c *Clients) IssueCard(ctx context.Context, req *gatewaypb.IssueCardRequest) (*gatewaypb.Card, error) {
	card, err := c.Gateway.IssueCard(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("issue card for tenant %s: %w", req.GetTenantId(), err)
	}
	return card, nil
}

// AdjustCardLimit changes an existing card's spend limit via the
// gateway, the same gateway.AdjustCardLimit RPC services/cli's
// "ration card adjust-limit" calls.
func (c *Clients) AdjustCardLimit(ctx context.Context, req *gatewaypb.AdjustCardLimitRequest) (*gatewaypb.Card, error) {
	card, err := c.Gateway.AdjustCardLimit(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("adjust limit for card %s: %w", req.GetCardId(), err)
	}
	return card, nil
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
