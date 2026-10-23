package main

import (
	"context"
	"fmt"

	"github.com/caelum0x/ration/pkg/mtls"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/cli/internal/authclient"
	"google.golang.org/grpc"
)

// clients bundles the two gRPC connections the CLI needs. Tenant/balance/
// agent-decision data lives in ledger-core (the authoritative store);
// card and deposit-simulation operations are fronted by the gateway,
// which is the only service allowed to talk to Airwallex and the only
// service that re-validates money-moving requests before proxying to
// ledger-core. See docs/ARCHITECTURE.md.
type clients struct {
	ledgerConn  *grpc.ClientConn
	gatewayConn *grpc.ClientConn

	Ledger  ledgerpb.LedgerCoreClient
	Gateway gatewaypb.GatewayClient
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
func dialClients(ctx context.Context, ledgerAddr, gatewayAddr, authAddr string) (*clients, error) {
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

	auth, err := authclient.Dial(authAddr)
	if err != nil {
		return nil, fmt.Errorf("dial auth at %s: %w", authAddr, err)
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

	return &clients{
		ledgerConn:  ledgerConn,
		gatewayConn: gatewayConn,
		Ledger:      ledgerpb.NewLedgerCoreClient(ledgerConn),
		Gateway:     gatewaypb.NewGatewayClient(gatewayConn),
	}, nil
}

func (c *clients) close() {
	if c == nil {
		return
	}
	_ = c.ledgerConn.Close()
	_ = c.gatewayConn.Close()
}
