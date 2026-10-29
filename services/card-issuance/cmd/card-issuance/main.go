// Command card-issuance is the entry point for the highest-value service
// in the Ration split (docs/SCALE-EXPANSION.md section 1,
// docs/adr/0004): Airwallex issuing-card management (internal/cardsvc)
// and the rationing/bridge-funding math (internal/bridge). It owns the
// Airwallex integration (mock or live per AIRWALLEX_MODE) and fronts
// ledger-core over gRPC — exactly like the gateway monolith did before
// this split, just as its own independently deployable, independently
// scalable unit. Stateless and idempotent per tenant, so it is safe to
// run as N replicas behind a load balancer.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/caelum0x/ration/pkg/metrics"
	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
	"github.com/caelum0x/ration/services/card-issuance/internal/authclient"
	"github.com/caelum0x/ration/services/card-issuance/internal/bridge"
	"github.com/caelum0x/ration/services/card-issuance/internal/cardsvc"
	"github.com/caelum0x/ration/services/card-issuance/internal/config"
	"github.com/caelum0x/ration/services/card-issuance/internal/grpcserver"
	"github.com/caelum0x/ration/services/card-issuance/internal/ledgerclient"
	"google.golang.org/grpc"
)

// serviceName/serviceVersion tag every trace span and log line this
// process emits (see pkg/telemetry).
const (
	serviceName    = "card-issuance"
	serviceVersion = "0.1.0"

	// operatorCredentialTTL bounds how long card-issuance's one minted
	// operator JWT (see internal/authclient.MintOperatorJWT) stays
	// valid. card-issuance is a long-running server, so this is
	// deliberately close to jwtauth.MaxTTL (24h) rather than a short
	// default, matching gateway/agent/tui's identical choice.
	operatorCredentialTTL = 20 * time.Hour
)

func main() {
	log := telemetry.NewLogger(serviceName)

	if err := run(log); err != nil {
		log.Error("card-issuance exited with error", "error", err)
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
	log.Info("config loaded", "airwallex_mode", cfg.AirwallexMode, "ledger_core_addr", cfg.LedgerCoreAddr, "auth_addr", cfg.AuthAddr, "mtls_enabled", cfg.MTLS.Enabled)

	// Mint a single operator-scoped JWT from auth and attach it to
	// every ledger-core call this process makes directly — see
	// internal/authclient's doc comment for why card-issuance needs
	// this: without it, every ApplyEntry call (bridge-funding
	// HOLD/RELEASE, card-limit DEBIT/RELEASE) starts failing the
	// moment RATION_LEDGER_REQUIRE_TENANT_AUTH is enforced.
	authClient, err := authclient.Dial(cfg.AuthAddr)
	if err != nil {
		return err
	}
	operatorToken, _, err := authClient.MintOperatorJWT(ctx, "ration-card-issuance", operatorCredentialTTL)
	authClient.Close()
	if err != nil {
		return err
	}

	// ledger-core's identity cert (see infra/dev-ca/) doubles as its
	// client cert, so card-issuance's own CARD_ISSUANCE_MTLS_* identity
	// is what's presented here; ServerName defaults to "ledger-core"
	// (the name infra/dev-ca/generate-dev-ca.sh issues ledger-core's cert
	// for) unless CARD_ISSUANCE_MTLS_SERVER_NAME overrides it.
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

	var awClient airwallex.Client
	switch cfg.AirwallexMode {
	case config.ModeLive:
		awClient = airwallex.NewLive(cfg.AirwallexBaseURL, cfg.AirwallexClientID, cfg.AirwallexAPIKey)
	default:
		awClient = airwallex.NewMock()
	}

	cardSvc := cardsvc.New(awClient, ledger)
	cardSvc.Metrics = met

	// Load any cards persisted by a prior run of this process and
	// persist every future mutation back to the same file — see
	// cardsvc.Service.EnablePersistence's doc comment for why this
	// matters: without it, a restart silently loses every issued
	// card's metadata while the ledger HOLD backing its limit lives on
	// in ledger-core forever, unreachable by any CloseCard/
	// AdjustCardLimit call ever again.
	cardsDataPath := filepath.Join(cfg.DataDir, "cards.json")
	if err := cardSvc.EnablePersistence(cardsDataPath); err != nil {
		return fmt.Errorf("card-issuance: load card store from %s: %w", cardsDataPath, err)
	}
	log.Info("card store persistence enabled", "path", cardsDataPath)

	bridgePolicy := bridge.Policy{
		MaxFraction:        cfg.BridgeMaxFraction,
		MinGrantMinorUnits: cfg.BridgeMinGrantMinorUnits,
		VelocityWindow:     cfg.BridgeVelocityWindow,
	}
	bridgeSvc := bridge.New(ledger, bridgePolicy)
	bridgeSvc.Metrics = met

	srv := grpcserver.New(cardSvc, bridgeSvc)

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
	cardissuancepb.RegisterCardIssuanceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	metricsSrv := met.Server(cfg.MetricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		log.Info("card-issuance gRPC server listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("card-issuance metrics server listening", "addr", cfg.MetricsAddr)
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
