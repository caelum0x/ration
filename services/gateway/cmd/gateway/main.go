// Command gateway is the entry point for the Go Airwallex gateway: it
// owns the Airwallex integration (mock or live per AIRWALLEX_MODE), fronts
// ledger-core over gRPC, and exposes gateway.proto's Gateway service plus
// (in live mode) a plain HTTP webhook listener.
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

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/airwallex"
	"github.com/caelum0x/ration/services/gateway/internal/bridge"
	"github.com/caelum0x/ration/services/gateway/internal/card"
	"github.com/caelum0x/ration/services/gateway/internal/config"
	"github.com/caelum0x/ration/services/gateway/internal/grpcserver"
	"github.com/caelum0x/ration/services/gateway/internal/httpserver"
	"github.com/caelum0x/ration/services/gateway/internal/ledgerclient"
	"google.golang.org/grpc"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("config loaded", "airwallex_mode", cfg.AirwallexMode, "ledger_core_addr", cfg.LedgerCoreAddr)

	ledger, err := ledgerclient.Dial(cfg.LedgerCoreAddr)
	if err != nil {
		return err
	}
	defer ledger.Close()

	var awClient airwallex.Client
	switch cfg.AirwallexMode {
	case config.ModeLive:
		awClient = airwallex.NewLive(cfg.AirwallexBaseURL, cfg.AirwallexClientID, cfg.AirwallexAPIKey, cfg.AirwallexWebhookSecret)
	default:
		awClient = airwallex.NewMock(cfg.AirwallexWebhookSecret)
	}

	bus := activity.NewBus()
	cardSvc := card.New(awClient, ledger)
	bridgePolicy := bridge.Policy{
		MaxFraction:        cfg.BridgeMaxFraction,
		MinGrantMinorUnits: cfg.BridgeMinGrantMinorUnits,
		VelocityWindow:     cfg.BridgeVelocityWindow,
	}
	bridgeSvc := bridge.New(ledger, bridgePolicy)

	srv := grpcserver.New(cardSvc, bridgeSvc, ledger, bus)

	grpcServer := grpc.NewServer()
	gatewaypb.RegisterGatewayServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GatewayGRPCAddr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		log.Info("gateway gRPC server listening", "addr", cfg.GatewayGRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	var httpSrv *http.Server
	if cfg.AirwallexMode == config.ModeLive {
		webhookHandler := httpserver.NewHandler(awClient, ledger, bus, log)
		httpSrv = &http.Server{
			Addr:              cfg.GatewayHTTPAddr,
			Handler:           webhookHandler.Mux(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("gateway webhook HTTP server listening", "addr", cfg.GatewayHTTPAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	} else {
		log.Info("skipping webhook HTTP listener: AIRWALLEX_MODE is not live")
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	grpcServer.GracefulStop()
	if httpSrv != nil {
		_ = httpSrv.Shutdown(shutdownCtx)
	}
	return nil
}
