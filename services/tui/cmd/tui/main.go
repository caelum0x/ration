// Command tui is the terminal dashboard judges watch Ration work in. It
// has no state of its own — everything comes from the gateway and
// ledger-core gRPC services over the addresses in GATEWAY_GRPC_ADDR and
// LEDGER_CORE_ADDR.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/caelum0x/ration/pkg/metrics"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/tui/internal/client"
	"github.com/caelum0x/ration/services/tui/internal/model"
)

// serviceName/serviceVersion tag every trace span this process emits.
// The TUI deliberately never logs to stdout/stderr once the bubbletea
// AltScreen program is running (it would corrupt the terminal display),
// so unlike every other Go service here it has no structured JSON
// logger — only tracing (pkg/telemetry.Setup) and a /metrics endpoint
// (pkg/metrics), both of which are silent background listeners.
const (
	serviceName    = "tui"
	serviceVersion = "0.1.0"
)

func main() {
	ctx := context.Background()

	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion)
	if err != nil {
		// Telemetry is best-effort observability, never a reason to
		// refuse to run the dashboard; report to stderr (safe pre-
		// AltScreen) and continue uninstrumented.
		fmt.Fprintf(os.Stderr, "ration-tui: telemetry setup failed, continuing without tracing: %v\n", err)
	}
	if shutdownTelemetry != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(shutdownCtx)
		}()
	}

	met := metrics.New(serviceName)
	metricsAddr := envOr("RATION_TUI_METRICS_ADDR", "localhost:9094")
	metricsSrv := met.Server(metricsAddr)
	go func() {
		_ = metricsSrv.ListenAndServe() // best-effort; stderr-silent so it never corrupts the AltScreen UI
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	gatewayAddr := envOr("GATEWAY_GRPC_ADDR", "localhost:7002")
	ledgerAddr := envOr("LEDGER_CORE_ADDR", "localhost:7001")

	clients, err := client.Dial(gatewayAddr, ledgerAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ration-tui: failed to set up gRPC clients: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if cerr := clients.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "ration-tui: error closing gRPC connections: %v\n", cerr)
		}
	}()

	p := tea.NewProgram(model.New(clients), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ration-tui: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
