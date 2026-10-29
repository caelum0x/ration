package main

import (
	"context"
	"fmt"

	"github.com/caelum0x/ration/services/cli/internal/moneyfmt"
	"github.com/caelum0x/ration/services/cli/internal/tenantregistryclient"
	"github.com/spf13/cobra"
)

// newTenantCmd is the real, missing operator path this closes:
// services/tenant-registry implements real tenant CRUD
// (proto/tenant_registry.proto's CreateTenant/GetTenant/ListTenants) and
// is started by every demo/deploy path alongside every other service
// (scripts/run-demo.sh, scripts/start-individually.sh,
// docker-compose.yml), but nothing in this repo ever dialed it —
// `ration seed` creates its 3 fixed demo tenants by calling
// ledger-core's own CreateTenant RPC directly (internal/demoseed),
// bypassing tenant-registry entirely, and no CLI/TUI/gateway command
// ever called it either. An operator could not onboard a new platform
// tenant into a running deployment at all without editing
// internal/demoseed.Tenants() in Go source and rebuilding the binary.
// `ration tenant create`/`ration tenant list` are tenant-registry's
// first real callers.
func newTenantCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Onboard and inspect platform tenants via tenant-registry",
	}
	cmd.AddCommand(newTenantCreateCmd(flags))
	cmd.AddCommand(newTenantListCmd(flags))
	return cmd
}

func newTenantCreateCmd(flags *globalFlags) *cobra.Command {
	var name, currency string
	var reserveFloorMinorUnits int64

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Onboard a new platform tenant via tenant-registry.CreateTenant",
		Long: `create calls tenant-registry.CreateTenant, which validates the
name/currency/reserve-floor and delegates persistence to ledger-core —
the same authoritative store "ration seed"'s 3 fixed demo tenants and
every card/deposit/bridge-funding RPC already read from.

Use this to onboard a tenant beyond the 3 demo tenants "ration seed"
creates, without editing Go source.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("tenant create: --name is required")
			}
			if len(currency) != 3 {
				return fmt.Errorf("tenant create: --currency must be a 3-letter ISO 4217 code, got %q", currency)
			}
			if reserveFloorMinorUnits < 0 {
				return fmt.Errorf("tenant create: --reserve-floor-minor-units must be >= 0, got %d", reserveFloorMinorUnits)
			}

			client, err := tenantregistryclient.Dial(flags.tenantRegistryAddr)
			if err != nil {
				return err
			}
			defer client.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout)
			defer cancel()

			t, err := client.CreateTenant(ctx, name, currency, reserveFloorMinorUnits)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "tenant_id:     %s\n", t.GetId())
			fmt.Fprintf(out, "name:          %s\n", t.GetName())
			fmt.Fprintf(out, "currency:      %s\n", t.GetCurrency())
			fmt.Fprintf(out, "reserve_floor: %s\n", moneyfmt.Minor(t.GetReserveFloorMinorUnits(), t.GetCurrency()))
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "tenant display name (required)")
	cmd.Flags().StringVar(&currency, "currency", "", "3-letter ISO 4217 settlement currency, e.g. USD (required)")
	cmd.Flags().Int64Var(&reserveFloorMinorUnits, "reserve-floor-minor-units", 0, "minimum balance (in minor units) this tenant's available balance must never drop below")

	return cmd
}

func newTenantListCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every tenant known to ledger-core via tenant-registry.ListTenants",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := tenantregistryclient.Dial(flags.tenantRegistryAddr)
			if err != nil {
				return err
			}
			defer client.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout)
			defer cancel()

			resp, err := client.ListTenants(ctx, 0, "")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			tenants := resp.GetTenants()
			if len(tenants) == 0 {
				fmt.Fprintln(out, "no tenants yet. Run \"ration seed\" or \"ration tenant create\".")
				return nil
			}

			tw := newTabWriter(out)
			fmt.Fprintln(tw, "TENANT_ID\tNAME\tCURRENCY\tRESERVE_FLOOR")
			for _, t := range tenants {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					sanitizeControlChars(t.GetId()),
					sanitizeControlChars(t.GetName()),
					sanitizeControlChars(t.GetCurrency()),
					moneyfmt.Minor(t.GetReserveFloorMinorUnits(), t.GetCurrency()),
				)
			}
			return tw.Flush()
		},
	}

	return cmd
}
