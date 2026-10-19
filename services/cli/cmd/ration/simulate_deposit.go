package main

import (
	"context"
	"fmt"
	"time"

	"github.com/caelum0x/ration/services/cli/internal/depositsim"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"github.com/spf13/cobra"
)

func newSimulateDepositCmd(flags *globalFlags) *cobra.Command {
	var (
		tenantID   string
		amount     int64
		late       bool
		expectedIn time.Duration
		lateBy     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "simulate-deposit",
		Short: "Simulate an incoming deposit for a tenant via the gateway",
		Long: `simulate-deposit calls the gateway's SimulateDeposit RPC to record a
deposit as if it had come in through Airwallex Global Accounts.

With --late, the deposit is recorded already past its expected arrival
time, so the agent's next poll of ledger-core's ListLateDeposits picks it
up as late and (if the tenant has pending card requests) runs the bridge
rationing decision described in docs/AGENT-DESIGN.md.

Examples:
  ration simulate-deposit --tenant t_abc123 --amount 50000
  ration simulate-deposit --tenant t_abc123 --amount 50000 --late`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if tenantID == "" {
				return fmt.Errorf("--tenant is required")
			}
			if amount <= 0 {
				return fmt.Errorf("--amount must be a positive number of minor units (e.g. cents), got %d", amount)
			}
			return runSimulateDeposit(cmd, flags, tenantID, amount, late, expectedIn, lateBy)
		},
	}

	cmd.Flags().StringVar(&tenantID, "tenant", "", "tenant ID (required)")
	cmd.Flags().Int64Var(&amount, "amount", 0, "deposit amount in minor units, e.g. cents (required)")
	cmd.Flags().BoolVar(&late, "late", false, "record the deposit as already past its expected arrival time")
	cmd.Flags().DurationVar(&expectedIn, "expected-in", depositsim.DefaultExpectedIn,
		"how far in the future an on-time deposit is expected (ignored with --late)")
	cmd.Flags().DurationVar(&lateBy, "late-by", depositsim.DefaultLateBy,
		"how far in the past to backdate a --late deposit's expected arrival time")

	return cmd
}

func runSimulateDeposit(cmd *cobra.Command, flags *globalFlags, tenantID string, amount int64, late bool, expectedIn, lateBy time.Duration) error {
	c, err := dialClients(flags.ledgerAddr, flags.gatewayAddr)
	if err != nil {
		return err
	}
	defer c.close()

	params := depositsim.Resolve(depositsim.Options{
		Now:        time.Now(),
		Late:       late,
		ExpectedIn: expectedIn,
		LateBy:     lateBy,
	})

	ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout)
	defer cancel()

	resp, err := c.Gateway.SimulateDeposit(ctx, &gatewaypb.SimulateDepositRequest{
		TenantId:         tenantID,
		AmountMinorUnits: amount,
		ExpectedAtUnix:   params.ExpectedAtUnix,
		MarkLate:         params.MarkLate,
	})
	if err != nil {
		return fmt.Errorf("simulate deposit for tenant %s: %w", tenantID, err)
	}

	out := cmd.OutOrStdout()
	if late {
		fmt.Fprintf(out, "Created LATE deposit %s for tenant %s: %d minor units, expected_at=%s (now - %s)\n",
			resp.DepositId, tenantID, amount, time.Unix(params.ExpectedAtUnix, 0).Format(time.RFC3339), lateBy)
		fmt.Fprintln(out, "This deposit is already overdue — the agent's next poll should pick it up as late.")
	} else {
		fmt.Fprintf(out, "Created deposit %s for tenant %s: %d minor units, expected_at=%s (now + %s)\n",
			resp.DepositId, tenantID, amount, time.Unix(params.ExpectedAtUnix, 0).Format(time.RFC3339), expectedIn)
	}
	return nil
}
