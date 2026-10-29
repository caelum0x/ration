package main

import (
	"context"
	"fmt"

	"github.com/caelum0x/ration/pkg/mtls"
	auditpb "github.com/caelum0x/ration/pkg/pb/audit"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/cli/internal/authclient"
	"google.golang.org/grpc"
)

// clients bundles the gRPC connections the CLI needs. Tenant/balance/
// agent-decision data lives in ledger-core (the authoritative store);
// card and deposit-simulation operations are fronted by the gateway,
// which is the only service allowed to talk to Airwallex and the only
// service that re-validates money-moving requests before proxying to
// ledger-core; audit-log is the independent, append-only mirror
// `audit-check` cross-checks ledger-core's own counts against. See
// docs/ARCHITECTURE.md.
type clients struct {
	ledgerConn  *grpc.ClientConn
	gatewayConn *grpc.ClientConn
	auditConn   *grpc.ClientConn

	Ledger  ledgerpb.LedgerCoreClient
	Gateway gatewaypb.GatewayClient
	Audit   auditpb.AuditLogClient
}

// dialAuthClient dials auth at authAddr, presenting the CLI's own
// CLI_MTLS_* identity (loaded fresh from the environment here, same as
// dialClients below) when CLI_MTLS_ENABLED is true. This is the one
// place every CLI subcommand that talks to auth directly — dialClients
// itself, and apikey.go/tenant.go's own standalone auth dials, which
// used to each call authclient.Dial(flags.authAddr) plain, bypassing
// mTLS entirely even when every other dial in the same binary used it —
// should go through, so a 6th unauthenticated-to-auth call site can't
// quietly reappear the same way the first one did. Without this, a
// plaintext dial against a real AUTH_MTLS_ENABLED=true auth instance
// doesn't degrade gracefully, it fails the TLS handshake outright
// ("error reading server preface"), confirmed live.
func dialAuthClient(authAddr string) (*authclient.Client, error) {
	mtlsCfg, err := mtls.LoadFromEnv("CLI")
	if err != nil {
		return nil, fmt.Errorf("load mTLS config: %w", err)
	}
	authDialOpt, err := mtlsCfg.WithDefaultServerName("auth").DialOption()
	if err != nil {
		return nil, fmt.Errorf("mTLS dial option for auth: %w", err)
	}
	auth, err := authclient.Dial(authAddr, authDialOpt)
	if err != nil {
		return nil, fmt.Errorf("dial auth at %s: %w", authAddr, err)
	}
	return auth, nil
}

// dialClients opens both gRPC connections. Callers must call close() when
// done, typically via defer.
//
// It mints a single operator-scoped JWT from auth at authAddr (see
// internal/authclient.MintOperatorJWT) and attaches it as an
// "authorization: Bearer <token>" credential on every subsequent
// gateway AND ledger-core RPC this connection makes — closing
// docs/THREAT-MODEL.md's "Known gaps" item 1's own named residual TODO
// for the CLI's half of it, including "the biggest one" sub-gap:
// ledger-core now verifies this same credential itself
// (services/ledger-core/src/tenantauth.rs), rather than trusting
// whatever already passed gateway's internal/tenantauth.
func dialClients(ctx context.Context, ledgerAddr, gatewayAddr, authAddr, auditAddr string) (*clients, error) {
	// Every RPC the CLI issues carries the one correlation ID main.go
	// mints for the whole command invocation (see
	// telemetry.ContextWithCorrelationID in main()) plus a trace span,
	// so "ration status --tenant X" shows up as a single trace spanning
	// every service it touches.
	commonDialOpts := []grpc.DialOption{
		telemetry.DialOption(),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(telemetry.StreamClientInterceptor()),
	}

	// mTLS (see docs/SCALE-EXPANSION.md section 8, pkg/mtls,
	// infra/dev-ca/) is opt-in and disabled by default — with no
	// CLI_MTLS_* env vars set, DialOption() falls back to plaintext, so
	// a bare `go run` keeps working with zero certs on disk. The CLI
	// dials two different peers with its one identity, so the server
	// name to verify each peer's certificate against is set per dial
	// (see mtls.Config.WithDefaultServerName).
	mtlsCfg, err := mtls.LoadFromEnv("CLI")
	if err != nil {
		return nil, fmt.Errorf("load mTLS config: %w", err)
	}

	auth, err := dialAuthClient(authAddr)
	if err != nil {
		return nil, err
	}
	defer auth.Close()
	token, _, err := auth.MintOperatorJWT(ctx, "ration-cli", operatorCredentialTTL)
	if err != nil {
		return nil, fmt.Errorf("mint operator credential from auth at %s: %w", authAddr, err)
	}
	credentialDialOpts := []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(authclient.UnaryClientInterceptor(token)),
		grpc.WithChainStreamInterceptor(authclient.StreamClientInterceptor(token)),
	}

	ledgerDialOpt, err := mtlsCfg.WithDefaultServerName("ledger-core").DialOption()
	if err != nil {
		return nil, fmt.Errorf("mTLS dial option for ledger-core: %w", err)
	}
	ledgerDialOpts := append(append([]grpc.DialOption{ledgerDialOpt}, credentialDialOpts...), commonDialOpts...)
	ledgerConn, err := grpc.NewClient(ledgerAddr, ledgerDialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial ledger-core at %s: %w", ledgerAddr, err)
	}

	gatewayDialOpt, err := mtlsCfg.WithDefaultServerName("gateway").DialOption()
	if err != nil {
		_ = ledgerConn.Close()
		return nil, fmt.Errorf("mTLS dial option for gateway: %w", err)
	}
	gatewayDialOpts := append(append([]grpc.DialOption{gatewayDialOpt}, credentialDialOpts...), commonDialOpts...)
	gatewayConn, err := grpc.NewClient(gatewayAddr, gatewayDialOpts...)
	if err != nil {
		_ = ledgerConn.Close()
		return nil, fmt.Errorf("dial gateway at %s: %w", gatewayAddr, err)
	}

	// audit-log has no tenant-scoped authorization of its own to
	// present a credential against (it is a sink, not a source of
	// truth for any one tenant's balance — see
	// services/audit-log/internal/grpcserver, which binds
	// source_service to the verified mTLS peer identity instead); the
	// credential is attached anyway for forward-compatibility and
	// consistency with every other dial in this function.
	auditDialOpt, err := mtlsCfg.WithDefaultServerName("audit-log").DialOption()
	if err != nil {
		_ = ledgerConn.Close()
		_ = gatewayConn.Close()
		return nil, fmt.Errorf("mTLS dial option for audit-log: %w", err)
	}
	auditDialOpts := append(append([]grpc.DialOption{auditDialOpt}, credentialDialOpts...), commonDialOpts...)
	auditConn, err := grpc.NewClient(auditAddr, auditDialOpts...)
	if err != nil {
		_ = ledgerConn.Close()
		_ = gatewayConn.Close()
		return nil, fmt.Errorf("dial audit-log at %s: %w", auditAddr, err)
	}

	return &clients{
		ledgerConn:  ledgerConn,
		gatewayConn: gatewayConn,
		auditConn:   auditConn,
		Ledger:      ledgerpb.NewLedgerCoreClient(ledgerConn),
		Gateway:     gatewaypb.NewGatewayClient(gatewayConn),
		Audit:       auditpb.NewAuditLogClient(auditConn),
	}, nil
}

func (c *clients) close() {
	if c == nil {
		return
	}
	_ = c.ledgerConn.Close()
	_ = c.gatewayConn.Close()
	_ = c.auditConn.Close()
}
