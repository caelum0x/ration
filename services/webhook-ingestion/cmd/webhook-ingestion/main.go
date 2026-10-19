// Command webhook-ingestion is the entry point for the service that
// receives Airwallex webhook deliveries (docs/SCALE-EXPANSION.md
// section 1, docs/adr/0004): verifies their HMAC signature
// (internal/airwallex), turns a "global_account.transaction.created"
// event into a MarkDepositReceived call against ledger-core
// (internal/webhookhandler), and republishes a normalized event onto
// internal/eventbus's Bus for internal subscribers (services/gateway)
// via webhook_ingestion.proto's StreamEvents RPC.
//
// Isolating webhook delivery (bursty, externally triggered, needs to be
// safe to retry independently of everything else) into its own
// deployable is the whole point of this split — a traffic spike or
// retry storm from Airwallex never competes with card-issuance's or
// tenant-registry's own deploy/traffic characteristics.
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
	webhookingestionpb "github.com/caelum0x/ration/pkg/pb/webhookingestion"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/config"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/eventbus"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/grpcserver"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/ledgerclient"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/ratelimit"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/webhookhandler"
	"google.golang.org/grpc"
)

// serviceName/serviceVersion tag every trace span and log line this
// process emits (see pkg/telemetry).
const (
	serviceName    = "webhook-ingestion"
	serviceVersion = "0.1.0"
)

func main() {
	log := telemetry.NewLogger(serviceName)

	if err := run(log); err != nil {
		log.Error("webhook-ingestion exited with error", "error", err)
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
	log.Info("config loaded", "ledger_core_addr", cfg.LedgerCoreAddr, "http_addr", cfg.HTTPAddr, "mtls_enabled", cfg.MTLS.Enabled,
		"webhook_secret_configured", cfg.AirwallexWebhookSecret != "")

	ledgerMTLS := cfg.MTLS
	if ledgerMTLS.ServerName == "" {
		ledgerMTLS.ServerName = "ledger-core"
	}
	ledgerDialCreds, err := ledgerMTLS.DialOption()
	if err != nil {
		return err
	}
	ledger, err := ledgerclient.Dial(cfg.LedgerCoreAddr,
		ledgerDialCreds,
		telemetry.DialOption(),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(telemetry.StreamClientInterceptor()),
	)
	if err != nil {
		return err
	}
	defer ledger.Close()

	bus := eventbus.NewBus()
	webhookHandler := webhookhandler.NewHandler(cfg.AirwallexWebhookSecret, ledger, bus, log)

	srv := grpcserver.New(bus)

	mtlsServerOpt, err := cfg.MTLS.ServerOption()
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		mtlsServerOpt,
		telemetry.ServerOption(),
		grpc.ChainUnaryInterceptor(telemetry.UnaryServerInterceptor(log), met.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(telemetry.StreamServerInterceptor(log), met.StreamServerInterceptor()),
	)
	webhookingestionpb.RegisterWebhookIngestionServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	metricsSrv := met.Server(cfg.MetricsAddr)

	// limiter sits in front of the entire plain-HTTP webhook listener,
	// keyed per source IP (see internal/ratelimit), so a flood of
	// (even validly-signed, replayed) webhook POSTs can't starve
	// legitimate Airwallex deliveries before they even reach signature
	// verification — docs/THREAT-MODEL.md's "Denial of service" row.
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

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           ratelimit.Middleware(limiter, webhookHandler.Mux()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 3)
	go func() {
		log.Info("webhook-ingestion gRPC server listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("webhook-ingestion metrics server listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		log.Info("webhook-ingestion webhook HTTP server listening", "addr", cfg.HTTPAddr)
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
