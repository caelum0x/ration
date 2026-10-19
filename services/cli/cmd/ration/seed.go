package main

import (
	"context"
	"fmt"
	"time"

	"github.com/caelum0x/ration/services/cli/internal/demoseed"
	"github.com/caelum0x/ration/services/cli/internal/moneyfmt"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/spf13/cobra"
)

func newSeedCmd(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "seed",
		Short: "Create 3 demo tenants (small/medium/large) in ledger-core",
		Long: `seed creates 3 demo tenants spanning a small, medium and large platform,
each with a different settlement currency and reserve floor, so the
rationing math in docs/AGENT-DESIGN.md has realistic variety to demo
against.

Safe to run multiple times — each run creates a fresh set of tenants with
new IDs; it does not deduplicate by name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSeed(cmd, flags)
		},
	}
}

func runSeed(cmd *cobra.Command, flags *globalFlags) error {
	c, err := dialClients(flags.ledgerAddr, flags.gatewayAddr)
	if err != nil {
		return err
	}
	defer c.close()

	out := cmd.OutOrStdout()
	tenants := demoseed.Tenants()

	fmt.Fprintf(out, "Seeding %d demo tenants against ledger-core at %s...\n\n", len(tenants), flags.ledgerAddr)

	for _, t := range tenants {
		ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout)
		created, err := c.Ledger.CreateTenant(ctx, &ledgerpb.CreateTenantRequest{
			Name:                   t.Name,
			Currency:               t.Currency,
			ReserveFloorMinorUnits: t.ReserveFloorMinorUnits,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("create tenant %q: %w", t.Name, err)
		}

		fmt.Fprintf(out, "  %-32s id=%-20s currency=%s reserve_floor=%s\n",
			created.Name, created.Id, created.Currency,
			moneyfmt.Minor(created.ReserveFloorMinorUnits, created.Currency))

		if err := fundOpeningCapital(cmd.Context(), c, flags.timeout, created.Id, t.OpeningCapitalMinorUnits, created.Currency); err != nil {
			return fmt.Errorf("fund opening capital for tenant %q: %w", t.Name, err)
		}
		fmt.Fprintf(out, "    funded with %s of settled opening capital (headroom %s above reserve floor)\n",
			moneyfmt.Minor(t.OpeningCapitalMinorUnits, created.Currency),
			moneyfmt.Minor(t.OpeningCapitalMinorUnits-created.ReserveFloorMinorUnits, created.Currency))
	}

	fmt.Fprintln(out, "\nDone. Use `ration status --tenant <id>` to inspect any of the tenants above.")
	return nil
}

// fundOpeningCapital gives a freshly seeded tenant real, spendable
// headroom before any demo scenario runs: it records a deposit for the
// tenant's opening capital and immediately marks it received, which
// credits ledger-core's balance for that tenant (see
// Ledger::mark_deposit_received in services/ledger-core). Without this,
// a brand-new tenant's available balance is exactly 0 — always below its
// reserve floor — so the agent's bridge envelope could never grant
// anything no matter how a later late-deposit scenario is sized.
func fundOpeningCapital(ctx context.Context, c *clients, timeout time.Duration, tenantID string, amountMinorUnits int64, currency string) error {
	if amountMinorUnits <= 0 {
		return nil
	}

	now := time.Now().Unix()

	rctx, cancel := context.WithTimeout(ctx, timeout)
	deposit, err := c.Ledger.RecordDeposit(rctx, &ledgerpb.RecordDepositRequest{
		TenantId: tenantID,
		ExpectedAmount: &commonpb.Money{
			MinorUnits: amountMinorUnits,
			Currency:   currency,
		},
		ExpectedAtUnix: now,
		ExternalRef:    "seed-opening-capital",
	})
	cancel()
	if err != nil {
		return fmt.Errorf("record opening capital deposit: %w", err)
	}

	rctx, cancel = context.WithTimeout(ctx, timeout)
	_, err = c.Ledger.MarkDepositReceived(rctx, &ledgerpb.MarkDepositReceivedRequest{
		DepositId:      deposit.GetId(),
		ReceivedAtUnix: now,
		ExternalRef:    "seed-opening-capital-settled",
	})
	cancel()
	if err != nil {
		return fmt.Errorf("mark opening capital deposit received: %w", err)
	}

	return nil
}
