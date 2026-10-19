// Command agent runs Ration's agentic rationing loop: on a fixed interval
// it asks ledger-core for every currently-late deposit and, for each one,
// runs the policy + LLM decision pipeline against the gateway.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gateway "github.com/caelum0x/ration/pkg/pb/gateway"
	ledger "github.com/caelum0x/ration/pkg/pb/ledger"

	"github.com/caelum0x/ration/services/agent/internal/ledgerclient"
	"github.com/caelum0x/ration/services/agent/internal/llm"
	"github.com/caelum0x/ration/services/agent/internal/loop"
	"github.com/caelum0x/ration/services/agent/internal/policy"
)

const (
	defaultLedgerCoreAddr = "localhost:7001"
	defaultGatewayAddr    = "localhost:7002"
	defaultPollInterval   = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ledgerAddr := envOrDefault("LEDGER_CORE_ADDR", defaultLedgerCoreAddr)
	gatewayAddr := envOrDefault("GATEWAY_GRPC_ADDR", defaultGatewayAddr)
	pollInterval := pollIntervalFromEnv(logger)

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		logger.Warn("ANTHROPIC_API_KEY is not set; LLM calls will fail until it is configured")
	}

	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer ledgerConn.Close()

	gatewayConn, err := grpc.NewClient(gatewayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer gatewayConn.Close()

	ledgerRaw := ledger.NewLedgerCoreClient(ledgerConn)
	gatewayClient := gateway.NewGatewayClient(gatewayConn)
	ledgerClient := ledgerclient.New(ledgerRaw)

	provider := llm.NewAnthropicProviderFromEnv()
	policyCfg := policy.LoadConfigFromEnv()

	runner := loop.NewRunner(ledgerClient, gatewayClient, provider, policyCfg, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("agent starting",
		"ledger_core_addr", ledgerAddr,
		"gateway_grpc_addr", gatewayAddr,
		"poll_interval", pollInterval.String(),
		"model", provider.Model,
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Run one tick immediately on startup instead of waiting a full
	// interval for the first scan.
	tick(ctx, runner, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("agent shutting down")
			return nil
		case <-ticker.C:
			tick(ctx, runner, logger)
		}
	}
}

func tick(ctx context.Context, runner *loop.Runner, logger *slog.Logger) {
	if err := runner.Tick(ctx); err != nil {
		logger.Error("tick failed", "error", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pollIntervalFromEnv(logger *slog.Logger) time.Duration {
	v := os.Getenv("RATION_AGENT_POLL_INTERVAL_SECONDS")
	if v == "" {
		return defaultPollInterval
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		logger.Warn("invalid RATION_AGENT_POLL_INTERVAL_SECONDS, using default", "value", v, "default", defaultPollInterval.String())
		return defaultPollInterval
	}
	return time.Duration(secs) * time.Second
}
