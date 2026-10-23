// Command tenant-registry is the entry point for tenant CRUD
// (docs/SCALE-EXPANSION.md section 1, docs/adr/0004): a thin service
// that owns no state of its own — every operation proxies to
// ledger-core, which remains the one authoritative store for tenant
// records. Split out purely so tenant CRUD's low, bursty-on-onboarding
// traffic and deploy cadence never forces a redeploy of card-issuance.
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
	tenantregistrypb "github.com/caelum0x/ration/pkg/pb/tenantregistry"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/tenant-registry/internal/authclient"
	"github.com/caelum0x/ration/services/tenant-registry/internal/config"
	"github.com/caelum0x/ration/services/tenant-registry/internal/grpcserver"
	"github.com/caelum0x/ration/services/tenant-registry/internal/ledgerclient"
	"github.com/caelum0x/ration/services/tenant-registry/internal/tenant"
	"google.golang.org/grpc"
)

// serviceName/serviceVersion tag every trace span and log line this
// process emits (see pkg/telemetry).
const (
	serviceName    = "tenant-registry"
	serviceVersion = "0.1.0"

	// operatorCredentialTTL bounds how long tenant-registry's one
	// minted operator JWT (see internal/authclient.MintOperatorJWT)
	// stays valid, matching every other long-running service's
	// identical choice.
	operatorCredentialTTL = 20 * time.Hour
)

func main() {
	log := telemetry.NewLogger(serviceName)

	if err := run(log); err != nil {
		log.Error("tenant-registry exited with error", "error", err)
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
	log.Info("config loaded", "ledger_core_addr", cfg.LedgerCoreAddr, "auth_addr", cfg.AuthAddr, "mtls_enabled", cfg.MTLS.Enabled)

	// Mint a single operator-scoped JWT from auth and attach it to
	// every ledger-core call this process makes directly — see
	// internal/authclient's doc comment for why: without it, every
	// CreateTenant/GetTenant/ListTenants call starts failing the
	// moment RATION_LEDGER_REQUIRE_TENANT_AUTH is enforced.
	authClient, err := authclient.Dial(cfg.AuthAddr)
	if err != nil {
		return err
	}
	operatorToken, _, err := authClient.MintOperatorJWT(ctx, "ration-tenant-registry", operatorCredentialTTL)
	authClient.Close()
	if err != nil {
		return err
	}

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
		grpc.WithChainUnaryInterceptor(authclient.UnaryClientInterceptor(operatorToken)),
		grpc.WithChainStreamInterceptor(authclient.StreamClientInterceptor(operatorToken)),
	)
	if err != nil {
		return err
	}
	defer ledger.Close()

	svc := tenant.New(ledger)
	srv := grpcserver.New(svc)

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
	tenantregistrypb.RegisterTenantRegistryServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	metricsSrv := met.Server(cfg.MetricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		log.Info("tenant-registry gRPC server listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("tenant-registry metrics server listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
	return nil
}
