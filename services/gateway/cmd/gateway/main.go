// Command gateway is the entry point for the thin BFF in front of
// services/auth, services/tenant-registry, services/card-issuance and
// services/webhook-ingestion (docs/SCALE-EXPANSION.md section 1,
// docs/adr/0004). It implements gateway.proto's Gateway service —
// unchanged, externally, from the pre-split monolith — by fanning out
// to services/card-issuance for every card and rationing-math RPC,
// dialing ledger-core directly only for SimulateDeposit, and merging
// services/webhook-ingestion's normalized event stream into its own
// activity feed.
//
// It also serves internal/httpapi, a REST/JSON facade over this exact
// same Gateway gRPC service (docs/SCALE-EXPANSION.md section 7,
// docs/adr/0003) on its own HTTP listener (cfg.HTTPAddr) — see that
// package's doc comment for the route list and ADR 0003 for why gRPC,
// not REST, remains the source of truth.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caelum0x/ration/pkg/metrics"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/authclient"
	"github.com/caelum0x/ration/services/gateway/internal/cardissuanceclient"
	"github.com/caelum0x/ration/services/gateway/internal/config"
	"github.com/caelum0x/ration/services/gateway/internal/grpcserver"
	"github.com/caelum0x/ration/services/gateway/internal/httpapi"
	"github.com/caelum0x/ration/services/gateway/internal/ledgerclient"
	"github.com/caelum0x/ration/services/gateway/internal/ratelimit"
	"github.com/caelum0x/ration/services/gateway/internal/tenantauth"
	"github.com/caelum0x/ration/services/gateway/internal/webhookeventsclient"
	"google.golang.org/grpc"
)

// serviceName/serviceVersion tag every trace span and log line this
// process emits (see pkg/telemetry).
const (
	serviceName    = "gateway"
	serviceVersion = "0.1.0"
)

func main() {
	log := telemetry.NewLogger(serviceName)

	if err := run(log); err != nil {
		log.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx := context.Background()

	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			log.Error("telemetry shutdown failed", "error", err)
		}
	}()

	met := metrics.New(serviceName)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("config loaded",
		"ledger_core_addr", cfg.LedgerCoreAddr,
		"card_issuance_addr", cfg.CardIssuanceAddr,
		"webhook_ingestion_addr", cfg.WebhookIngestionAddr,
		"auth_addr", cfg.AuthAddr,
		"require_tenant_auth", cfg.RequireTenantAuth,
		"mtls_enabled", cfg.MTLS.Enabled)

	clientDialOpts := []grpc.DialOption{
		telemetry.DialOption(),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(telemetry.StreamClientInterceptor()),
	}

	// ledger-core's identity cert (see infra/dev-ca/) doubles as its
	// client cert, so gateway's own GATEWAY_MTLS_* identity is what's
	// presented here; ServerName defaults to "ledger-core" (the name
	// infra/dev-ca/generate-dev-ca.sh issues ledger-core's cert for)
	// unless GATEWAY_MTLS_SERVER_NAME overrides it.
	ledgerMTLS := cfg.MTLS
	if ledgerMTLS.ServerName == "" {
		ledgerMTLS.ServerName = "ledger-core"
	}
	ledgerDialCreds, err := ledgerMTLS.DialOption()
	if err != nil {
		return err
	}
	ledger, err := ledgerclient.Dial(cfg.LedgerCoreAddr, append([]grpc.DialOption{ledgerDialCreds}, clientDialOpts...)...)
	if err != nil {
		return err
	}
	defer ledger.Close()

	cardIssuanceMTLS := cfg.MTLS
	cardIssuanceMTLS.ServerName = "card-issuance"
	cardIssuanceDialCreds, err := cardIssuanceMTLS.DialOption()
	if err != nil {
		return err
	}
	cards, err := cardissuanceclient.Dial(cfg.CardIssuanceAddr, append([]grpc.DialOption{cardIssuanceDialCreds}, clientDialOpts...)...)
	if err != nil {
		return err
	}
	defer cards.Close()

	webhookIngestionMTLS := cfg.MTLS
	webhookIngestionMTLS.ServerName = "webhook-ingestion"
	webhookIngestionDialCreds, err := webhookIngestionMTLS.DialOption()
	if err != nil {
		return err
	}
	webhookEvents, err := webhookeventsclient.Dial(cfg.WebhookIngestionAddr, append([]grpc.DialOption{webhookIngestionDialCreds}, clientDialOpts...)...)
	if err != nil {
		return err
	}
	defer webhookEvents.Close()

	// auth is dialed unconditionally (same pattern as the three clients
	// above — gRPC's dial is lazy, so this doesn't block startup even
	// when cfg.RequireTenantAuth is false) so that flipping
	// RATION_GATEWAY_REQUIRE_TENANT_AUTH on is a config change, not a
	// redeploy. See internal/tenantauth's package doc for what this
	// closes (docs/THREAT-MODEL.md's "Known gaps" item 1).
	authMTLS := cfg.MTLS
	authMTLS.ServerName = "auth"
	authDialCreds, err := authMTLS.DialOption()
	if err != nil {
		return err
	}
	auth, err := authclient.Dial(cfg.AuthAddr, append([]grpc.DialOption{authDialCreds}, clientDialOpts...)...)
	if err != nil {
		return err
	}
	defer auth.Close()

	bus := activity.NewBus()

	// Merge services/webhook-ingestion's normalized event stream (e.g.
	// "deposit received" from a real Airwallex webhook delivery) into
	// this process's own activity feed for the lifetime of the gateway
	// — see internal/webhookeventsclient's doc comment.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go webhookEvents.RelayInto(relayCtx, bus, log)

	srv := grpcserver.New(cards, ledger, bus)

	// limiter sits in front of every RPC on this gRPC server (gateway's
	// entire public-facing surface — see internal/ratelimit), keyed per
	// tenant so one noisy or compromised caller can't starve every other
	// tenant's requests here.
	limiter := ratelimit.New(cfg.RateLimitRPS, cfg.RateLimitBurst)
	sweepStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(cfg.RateLimitSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				limiter.Sweep(cfg.RateLimitSweepInterval * 3)
			case <-sweepStop:
				return
			}
		}
	}()
	defer close(sweepStop)

	mtlsServerOpt, err := cfg.MTLS.ServerOption()
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		mtlsServerOpt,
		telemetry.ServerOption(),
		grpc.ChainUnaryInterceptor(
			telemetry.UnaryServerInterceptor(log),
			met.UnaryServerInterceptor(),
			ratelimit.UnaryServerInterceptor(limiter),
			tenantauth.UnaryServerInterceptor(auth, cfg.RequireTenantAuth),
		),
		grpc.ChainStreamInterceptor(telemetry.StreamServerInterceptor(log), met.StreamServerInterceptor(), ratelimit.StreamServerInterceptor(limiter)),
	)
	gatewaypb.RegisterGatewayServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GatewayGRPCAddr)
	if err != nil {
		return err
	}

	metricsSrv := met.Server(cfg.MetricsAddr)

	// internal/httpapi (docs/SCALE-EXPANSION.md section 7, docs/adr/0003)
	// is a REST/JSON facade in front of this same gRPC server: it holds
	// a real gatewaypb.GatewayClient dialing this process's own gRPC
	// listener above (GatewayGRPCDialAddr, a loopback address — see
	// internal/config's doc comment on why that's distinct from the
	// GatewayGRPCAddr bind address), so a REST request takes the exact
	// same code path through grpcserver.Server a gRPC request would,
	// never a second implementation of gateway.proto's RPCs.
	selfMTLS := cfg.MTLS.WithDefaultServerName("gateway")
	selfDialCreds, err := selfMTLS.DialOption()
	if err != nil {
		return err
	}
	selfConn, err := grpc.NewClient(cfg.GatewayGRPCDialAddr, append([]grpc.DialOption{selfDialCreds}, clientDialOpts...)...)
	if err != nil {
		return err
	}
	defer selfConn.Close()

	restAPI := httpapi.NewHandler(gatewaypb.NewGatewayClient(selfConn), log)
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           restAPI.Mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 3)
	go func() {
		log.Info("gateway gRPC server listening", "addr", cfg.GatewayGRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("gateway metrics server listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		log.Info("gateway REST facade listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	grpcServer.GracefulStop()
	_ = metricsSrv.Shutdown(shutdownCtx)
	_ = httpSrv.Shutdown(shutdownCtx)
	return nil
}
