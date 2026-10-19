// Command agent runs Ration's agentic rationing loop: on a fixed interval
// it asks ledger-core for every currently-late deposit and, for each one,
// runs the policy + LLM decision pipeline against the gateway.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/caelum0x/ration/pkg/metrics"
	"github.com/caelum0x/ration/pkg/mtls"
	gateway "github.com/caelum0x/ration/pkg/pb/gateway"
	ledger "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/pkg/telemetry"

	"github.com/caelum0x/ration/services/agent/internal/ledgerclient"
	"github.com/caelum0x/ration/services/agent/internal/llm"
	"github.com/caelum0x/ration/services/agent/internal/loop"
	"github.com/caelum0x/ration/services/agent/internal/policy"
)

const (
	defaultLedgerCoreAddr = "localhost:7001"
	defaultGatewayAddr    = "localhost:7002"
	defaultPollInterval   = 10 * time.Second
	defaultMetricsAddr    = "localhost:9093"

	serviceName    = "agent"
	serviceVersion = "0.1.0"
)

func main() {
	logger := telemetry.NewLogger(serviceName)
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	bootCtx := context.Background()

	shutdownTelemetry, err := telemetry.Setup(bootCtx, serviceName, serviceVersion)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Error("telemetry shutdown failed", "error", err)
		}
	}()

	met := metrics.New(serviceName)
	metricsAddr := envOrDefault("RATION_AGENT_METRICS_ADDR", defaultMetricsAddr)
	metricsSrv := met.Server(metricsAddr)
	go func() {
		logger.Info("agent metrics server listening", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server exited with error", "error", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	ledgerAddr := envOrDefault("LEDGER_CORE_ADDR", defaultLedgerCoreAddr)
	gatewayAddr := envOrDefault("GATEWAY_GRPC_ADDR", defaultGatewayAddr)
	pollInterval := pollIntervalFromEnv(logger)

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		logger.Warn("ANTHROPIC_API_KEY is not set; LLM calls will fail until it is configured")
	}

	// mTLS (see docs/SCALE-EXPANSION.md section 8, pkg/mtls,
	// infra/dev-ca/) is opt-in and disabled by default — with no
	// AGENT_MTLS_* env vars set, mtlsCfg.Enabled is false and every
	// DialOption() below falls back to plaintext, so a bare `go run`
	// keeps working with zero certs on disk. The agent dials two
	// different peers with its one identity, so the server name to
	// verify each peer's certificate against is set per dial (see
	// mtls.Config.WithDefaultServerName) rather than once.
	mtlsCfg, err := mtls.LoadFromEnv("AGENT")
	if err != nil {
		return err
	}

	commonDialOpts := []grpc.DialOption{
		telemetry.DialOption(),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(telemetry.StreamClientInterceptor()),
	}

	ledgerDialOpt, err := mtlsCfg.WithDefaultServerName("ledger-core").DialOption()
	if err != nil {
		return err
	}
	ledgerConn, err := grpc.NewClient(ledgerAddr, append([]grpc.DialOption{ledgerDialOpt}, commonDialOpts...)...)
	if err != nil {
		return err
	}
	defer ledgerConn.Close()

	gatewayDialOpt, err := mtlsCfg.WithDefaultServerName("gateway").DialOption()
	if err != nil {
		return err
	}
	gatewayConn, err := grpc.NewClient(gatewayAddr, append([]grpc.DialOption{gatewayDialOpt}, commonDialOpts...)...)
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

// tick runs one poll-and-decide cycle as the root of its own trace and
// correlation ID: everything this tick causes downstream (RPCs to
// ledger-core and gateway, which themselves call ledger-core) carries
// the same trace/correlation ID all the way through, so one tick's full
// fan-out is visible as a single trace in Grafana/Tempo and a single
// grep in aggregated logs.
func tick(ctx context.Context, runner *loop.Runner, logger *slog.Logger) {
	ctx = telemetry.ContextWithCorrelationID(ctx, telemetry.NewCorrelationID())
	err := telemetry.WithSpan(ctx, "agent", "agent.tick", runner.Tick)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "tick failed", append(telemetry.LogAttrs(ctx), slog.String("error", err.Error()))...)
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
