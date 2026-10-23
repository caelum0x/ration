package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/spf13/cobra"
)

// serviceName/serviceVersion tag every trace span this invocation
// emits. "cli" is not a long-running process — telemetry.Setup still
// installs a real OTLP exporter per invocation (batched, best-effort,
// non-blocking per pkg/telemetry's doc comment) and its shutdown func
// below flushes the one trace before the process exits.
const (
	serviceName    = "cli"
	serviceVersion = "0.1.0"
)

const (
	defaultLedgerAddr  = "localhost:7001"
	defaultGatewayAddr = "localhost:7002"
	defaultAuthAddr    = "localhost:7010"
	defaultAuditAddr   = "localhost:7003"
	defaultRPCTimeout  = 10 * time.Second

	// operatorCredentialTTL bounds how long the CLI's one minted
	// operator JWT (see internal/authclient.MintOperatorJWT) stays
	// valid. A CLI invocation is always a single short-lived process, so
	// this only needs to outlive the longest realistic command — well
	// under jwtauth.MaxTTL (24h) — not the lifetime of a long-running
	// service.
	operatorCredentialTTL = time.Hour
)

// globalFlags holds the persistent flag values shared by every subcommand.
type globalFlags struct {
	ledgerAddr  string
	gatewayAddr string
	authAddr    string
	auditAddr   string
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
	root.PersistentFlags().StringVar(&flags.authAddr, "auth-addr",
		envOrDefault("AUTH_GRPC_ADDR", defaultAuthAddr),
		"auth gRPC address, used to mint this invocation's operator credential (env AUTH_GRPC_ADDR)")
	root.PersistentFlags().StringVar(&flags.auditAddr, "audit-addr",
		envOrDefault("AUDIT_LOG_GRPC_ADDR", defaultAuditAddr),
		"audit-log gRPC address, used by audit-check (env AUDIT_LOG_GRPC_ADDR)")
	root.PersistentFlags().DurationVar(&flags.timeout, "timeout", defaultRPCTimeout,
		"per-RPC timeout")

	root.AddCommand(
		newSeedCmd(flags),
		newSimulateDepositCmd(flags),
		newStatusCmd(flags),
		newAuditCheckCmd(flags),
	)

	return root
}

func main() {
	ctx := context.Background()

	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion)
	if err != nil {
		// Never let an unreachable/misconfigured collector block the
		// operator CLI from running; report and continue uninstrumented.
		fmt.Fprintf(os.Stderr, "ration: telemetry setup failed, continuing without tracing: %v\n", err)
	} else {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(shutdownCtx)
		}()
	}

	// One correlation ID per CLI invocation, threaded through every
	// subcommand's cmd.Context() and from there into every gRPC call
	// dialClients makes — this is the "correlation ID threaded from the
	// CLI command through every hop" requirement from
	// docs/SCALE-EXPANSION.md section 2.
	ctx = telemetry.ContextWithCorrelationID(ctx, telemetry.NewCorrelationID())

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ration: %v\n", err)
		os.Exit(1)
	}
}
