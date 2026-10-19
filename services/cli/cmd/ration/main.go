package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultLedgerAddr  = "localhost:7001"
	defaultGatewayAddr = "localhost:7002"
	defaultRPCTimeout  = 10 * time.Second
)

// globalFlags holds the persistent flag values shared by every subcommand.
type globalFlags struct {
	ledgerAddr  string
	gatewayAddr string
	timeout     time.Duration
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newRootCmd() *cobra.Command {
	flags := &globalFlags{}

	root := &cobra.Command{
		Use:   "ration",
		Short: "Operator CLI for the Ration agentic spend controller",
		Long: `ration is the operator CLI for the Ration platform spend controller.

It drives the same gateway and ledger-core services the agent and TUI use,
so judges can seed demo data, force a late-deposit scenario, and inspect
the resulting agent decisions — all live, from the terminal.`,
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&flags.ledgerAddr, "ledger-addr",
		envOrDefault("LEDGER_CORE_ADDR", defaultLedgerAddr),
		"ledger-core gRPC address (env LEDGER_CORE_ADDR)")
	root.PersistentFlags().StringVar(&flags.gatewayAddr, "gateway-addr",
		envOrDefault("GATEWAY_GRPC_ADDR", defaultGatewayAddr),
		"gateway gRPC address (env GATEWAY_GRPC_ADDR)")
	root.PersistentFlags().DurationVar(&flags.timeout, "timeout", defaultRPCTimeout,
		"per-RPC timeout")

	root.AddCommand(
		newSeedCmd(flags),
		newSimulateDepositCmd(flags),
		newStatusCmd(flags),
	)

	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "ration: %v\n", err)
		os.Exit(1)
	}
}
