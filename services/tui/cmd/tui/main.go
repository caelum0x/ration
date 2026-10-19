// Command tui is the terminal dashboard judges watch Ration work in. It
// has no state of its own — everything comes from the gateway and
// ledger-core gRPC services over the addresses in GATEWAY_GRPC_ADDR and
// LEDGER_CORE_ADDR.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/caelum0x/ration/services/tui/internal/client"
	"github.com/caelum0x/ration/services/tui/internal/model"
)

func main() {
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
