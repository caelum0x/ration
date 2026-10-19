package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/caelum0x/ration/services/cli/internal/moneyfmt"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/spf13/cobra"
)

const defaultDecisionLimit = 10

func newStatusCmd(flags *globalFlags) *cobra.Command {
	var (
		tenantID      string
		decisionLimit int32
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a tenant's balance, cards, and recent agent decisions",
		Long: `status prints a single tenant's current state: ledger-core's balance
(available + held), the gateway's view of that tenant's cards, and the
most recent agent decisions recorded via RecordAgentDecision — the same
audit trail the TUI's activity feed renders live.

Run this right after "ration simulate-deposit --late" to see what the
agent decided.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if tenantID == "" {
				return fmt.Errorf("--tenant is required")
			}
			return runStatus(cmd, flags, tenantID, decisionLimit)
		},
	}

	cmd.Flags().StringVar(&tenantID, "tenant", "", "tenant ID (required)")
	cmd.Flags().Int32Var(&decisionLimit, "decisions", defaultDecisionLimit, "max number of recent agent decisions to show")

	return cmd
}

func runStatus(cmd *cobra.Command, flags *globalFlags, tenantID string, decisionLimit int32) error {
	c, err := dialClients(flags.ledgerAddr, flags.gatewayAddr)
	if err != nil {
		return err
	}
	defer c.close()

	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	tenant, err := fetchTenant(ctx, c, flags.timeout, tenantID)
	if err != nil {
		return err
	}
	printTenantHeader(out, tenant)

	balance, err := fetchBalance(ctx, c, flags.timeout, tenantID)
	if err != nil {
		return err
	}
	printBalance(out, balance)

	cards, err := fetchCards(ctx, c, flags.timeout, tenantID)
	if err != nil {
		return err
	}
	printCards(out, cards)

	decisions, err := fetchDecisions(ctx, c, flags.timeout, tenantID, decisionLimit)
	if err != nil {
		return err
	}
	printDecisions(out, decisions)

	return nil
}

func fetchTenant(ctx context.Context, c *clients, timeout time.Duration, tenantID string) (*ledgerpb.Tenant, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tenant, err := c.Ledger.GetTenant(rctx, &ledgerpb.GetTenantRequest{Id: tenantID})
	if err != nil {
		return nil, fmt.Errorf("get tenant %s: %w", tenantID, err)
	}
	return tenant, nil
}

func fetchBalance(ctx context.Context, c *clients, timeout time.Duration, tenantID string) (*ledgerpb.Balance, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	balance, err := c.Ledger.GetBalance(rctx, &ledgerpb.GetBalanceRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("get balance for tenant %s: %w", tenantID, err)
	}
	return balance, nil
}

func fetchCards(ctx context.Context, c *clients, timeout time.Duration, tenantID string) ([]*gatewaypb.Card, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.Gateway.ListCards(rctx, &gatewaypb.ListCardsRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("list cards for tenant %s: %w", tenantID, err)
	}
	return resp.Cards, nil
}

func fetchDecisions(ctx context.Context, c *clients, timeout time.Duration, tenantID string, limit int32) ([]*ledgerpb.AgentDecision, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.Ledger.ListAgentDecisions(rctx, &ledgerpb.ListAgentDecisionsRequest{TenantId: tenantID, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list agent decisions for tenant %s: %w", tenantID, err)
	}
	return resp.Decisions, nil
}

func printTenantHeader(out io.Writer, tenant *ledgerpb.Tenant) {
	w := newTabWriter(out)
	fmt.Fprintf(w, "Tenant\t%s\n", tenant.Name)
	fmt.Fprintf(w, "ID\t%s\n", tenant.Id)
	fmt.Fprintf(w, "Currency\t%s\n", tenant.Currency)
	fmt.Fprintf(w, "Reserve floor\t%s\n", moneyfmt.Minor(tenant.ReserveFloorMinorUnits, tenant.Currency))
	_ = w.Flush()
	fmt.Fprintln(out)
}

func printBalance(out io.Writer, balance *ledgerpb.Balance) {
	fmt.Fprintln(out, "Balance")
	w := newTabWriter(out)
	fmt.Fprintf(w, "  Available\t%s\n", moneyfmt.Minor(balance.AvailableMinorUnits, balance.Currency))
	fmt.Fprintf(w, "  Held\t%s\n", moneyfmt.Minor(balance.HeldMinorUnits, balance.Currency))
	_ = w.Flush()
	fmt.Fprintln(out)
}

func printCards(out io.Writer, cards []*gatewaypb.Card) {
	fmt.Fprintf(out, "Cards (%d)\n", len(cards))
	if len(cards) == 0 {
		fmt.Fprintln(out, "  (none)")
		fmt.Fprintln(out)
		return
	}

	w := newTabWriter(out)
	fmt.Fprintln(w, "  CARDHOLDER\tSTATUS\tLIMIT\tINTERVAL\tCARD ID")
	for _, card := range cards {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
			card.CardholderName,
			card.Status.String(),
			moneyfmt.Minor(card.LimitMinorUnits, card.Currency),
			card.LimitInterval,
			card.Id,
		)
	}
	_ = w.Flush()
	fmt.Fprintln(out)
}

func printDecisions(out io.Writer, decisions []*ledgerpb.AgentDecision) {
	fmt.Fprintf(out, "Recent agent decisions (%d)\n", len(decisions))
	if len(decisions) == 0 {
		fmt.Fprintln(out, "  (none yet — try `ration simulate-deposit --late` then wait for the agent's next poll)")
		return
	}

	w := newTabWriter(out)
	fmt.Fprintln(w, "  WHEN\tTRIGGER\tACTION\tRATIONALE")
	for _, d := range decisions {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
			time.Unix(d.CreatedAtUnix, 0).Format(time.RFC3339),
			d.Trigger,
			d.Action.String(),
			truncate(d.Rationale, 80),
		)
	}
	_ = w.Flush()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func newTabWriter(out io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
}
