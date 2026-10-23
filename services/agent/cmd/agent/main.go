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

	"github.com/caelum0x/ration/services/agent/internal/authclient"
	"github.com/caelum0x/ration/services/agent/internal/ledgerclient"
	"github.com/caelum0x/ration/services/agent/internal/llm"
	"github.com/caelum0x/ration/services/agent/internal/loop"
	"github.com/caelum0x/ration/services/agent/internal/policy"
)

const (
	defaultLedgerCoreAddr = "localhost:7001"
	defaultGatewayAddr    = "localhost:7002"
	defaultAuthAddr       = "localhost:7010"
	defaultPollInterval   = 10 * time.Second
	defaultMetricsAddr    = "localhost:9093"

	serviceName    = "agent"
	serviceVersion = "0.1.0"

	// operatorCredentialTTL bounds how long the agent's one minted
	// operator JWT (see internal/authclient.MintOperatorJWT) stays
	// valid. The agent is a long-running poll loop, so — like the TUI —
	// this is deliberately close to jwtauth.MaxTTL (24h) rather than a
	// short default.
	operatorCredentialTTL = 20 * time.Hour
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
	authAddr := envOrDefault("AUTH_GRPC_ADDR", defaultAuthAddr)
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

	// Mint a single operator-scoped JWT from auth and attach it to
	// every gateway AND ledger-core RPC this process makes (see
	// internal/authclient's doc comment for why the agent needs
	// operator scope, not a tenant-scoped credential) — without this,
	// RationBridgeFunding/IssueCard/AdjustCardLimit would start failing
	// the moment RATION_GATEWAY_REQUIRE_TENANT_AUTH is enforced, and
	// every ledger-core call (ListLateDeposits, RecordAgentDecision,
	// ...) would start failing the moment
	// RATION_LEDGER_REQUIRE_TENANT_AUTH is enforced — breaking the
	// agent's entire decision loop, not just hardening it.
	authClient, err := authclient.Dial(authAddr)
	if err != nil {
		return err
	}
	operatorToken, _, err := authClient.MintOperatorJWT(bootCtx, "ration-agent", operatorCredentialTTL)
	authClient.Close()
	if err != nil {
		return err
	}
	credentialDialOpts := []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(authclient.UnaryClientInterceptor(operatorToken)),
		grpc.WithChainStreamInterceptor(authclient.StreamClientInterceptor(operatorToken)),
	}

	ledgerDialOpt, err := mtlsCfg.WithDefaultServerName("ledger-core").DialOption()
	if err != nil {
		return err
	}
	ledgerDialOpts := append(append([]grpc.DialOption{ledgerDialOpt}, credentialDialOpts...), commonDialOpts...)
	ledgerConn, err := grpc.NewClient(ledgerAddr, ledgerDialOpts...)
	if err != nil {
		return err
	}
	defer ledgerConn.Close()

	gatewayDialOpt, err := mtlsCfg.WithDefaultServerName("gateway").DialOption()
	if err != nil {
		return err
	}
	gatewayDialOpts := append(append([]grpc.DialOption{gatewayDialOpt}, credentialDialOpts...), commonDialOpts...)
	gatewayConn, err := grpc.NewClient(gatewayAddr, gatewayDialOpts...)
	if err != nil {
		return err
	}
	defer gatewayConn.Close()

	ledgerRaw := ledger.NewLedgerCoreClient(ledgerConn)
	gatewayClient := gateway.NewGatewayClient(gatewayConn)
	ledgerClient := ledgerclient.New(ledgerRaw)

	provider := llm.NewAnthropicProviderFromEnv()
	policyCfg := policy.LoadConfigFromEnv()

	// Wrap the real provider in a circuit breaker (see internal/llm/
	// breaker.go, docs/AGENT-DESIGN.md: "there is no confirmed... no
	// circuit breaker: a slow/erroring LLM provider stalls that tick's
	// decision loop") so a stuck Anthropic/Relay call can never block
	// a tick beyond RATION_AGENT_LLM_BREAKER_COOLDOWN_SECONDS — once
	// tripped, internal/loop.Runner executes the already-safe
	// deterministic hard-rule envelope directly instead of waiting.
	llmProvider := llm.NewCircuitBreakerFromEnv(provider, logger)

	runner := loop.NewRunner(ledgerClient, gatewayClient, llmProvider, policyCfg, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("agent starting",
		"ledger_core_addr", ledgerAddr,
		"gateway_grpc_addr", gatewayAddr,
		"auth_grpc_addr", authAddr,
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
