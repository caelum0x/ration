package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/idgen"
	"github.com/caelum0x/ration/services/gateway/internal/ledgerclient"
)

// runSetup creates a fresh tenant with the given reserve floor, credits
// it with --fund minor units (so the subsequent load generator's
// DEBIT/HOLD ops have real headroom to actually contend over instead of
// bouncing off a zero balance every time), and prints exactly the new
// tenant ID to stdout — nothing else — so the calling shell script can
// capture it with `TENANT_ID="$("$LOADER_BIN" setup ...)"`.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	addr := fs.String("addr", "", "ledger-core gRPC address")
	reserveFloor := fs.Int64("reserve-floor", 10000, "tenant reserve floor, minor units")
	fund := fs.Int64("fund", 1_000_000, "initial CREDIT to seed available balance, minor units")
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
	client.Timeout = 10 * time.Second

	ctx := context.Background()
	tenant, err := client.CreateTenant(ctx, &ledgerpb.CreateTenantRequest{
		Name:                   idgen.New("chaos-tenant"),
		Currency:               "USD",
		ReserveFloorMinorUnits: *reserveFloor,
	})
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}

	if *fund > 0 {
		if _, err := client.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
			TenantId: tenant.GetId(),
			Type:     commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_CREDIT,
			Amount: &commonpb.Money{
				MinorUnits: *fund,
				Currency:   "USD",
			},
			Reference:      "chaos-setup-fund",
			IdempotencyKey: "chaos-setup-fund-" + tenant.GetId(),
			Memo:           "chaos test initial funding",
		}); err != nil {
			return fmt.Errorf("fund tenant: %w", err)
		}
	}

	fmt.Println(tenant.GetId())
	return nil
}
