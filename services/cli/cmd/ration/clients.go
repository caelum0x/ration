package main

import (
	"fmt"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
func dialClients(ledgerAddr, gatewayAddr string) (*clients, error) {
	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial ledger-core at %s: %w", ledgerAddr, err)
	}

	gatewayConn, err := grpc.NewClient(gatewayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
