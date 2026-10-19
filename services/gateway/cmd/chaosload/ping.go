package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/ledgerclient"
)

// runPing dials addr and makes one cheap, read-only RPC with a short
// timeout. Used by the chaos shell script's readiness-polling loop to
// detect "ledger-core has finished WAL recovery and is accepting RPCs
// again" after each restart, without assuming any tenant exists yet.
func runPing(args []string) error {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	addr := fs.String("addr", "", "ledger-core gRPC address, e.g. 127.0.0.1:7099")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" {
		return fmt.Errorf("--addr is required")
	}

	client, err := ledgerclient.Dial(*addr)
	if err != nil {
		return err
	}
	defer client.Close()
	client.Timeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = client.ListTenants(ctx, &ledgerpb.ListTenantsRequest{PageSize: 1})
	return err
}
