// Command auth is the entry point for the service that issues/verifies
// tenant API keys, mints/verifies JWTs for the TUI/CLI, and signs
// service mTLS certificates at runtime (docs/SCALE-EXPANSION.md section
// 1, docs/adr/0004) — new capability, not an extraction of existing
// gateway behavior: nothing in this project previously did any of this.
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
	authpb "github.com/caelum0x/ration/pkg/pb/auth"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/auth/internal/apikey"
	"github.com/caelum0x/ration/services/auth/internal/config"
	"github.com/caelum0x/ration/services/auth/internal/grpcserver"
	"github.com/caelum0x/ration/services/auth/internal/jwtauth"
	"github.com/caelum0x/ration/services/auth/internal/servicecertca"
	"google.golang.org/grpc"
)

// serviceName/serviceVersion tag every trace span and log line this
// process emits (see pkg/telemetry).
const (
	serviceName    = "auth"
	serviceVersion = "0.1.0"
)

func main() {
	log := telemetry.NewLogger(serviceName)

	if err := run(log); err != nil {
		log.Error("auth exited with error", "error", err)
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
	log.Info("config loaded", "grpc_addr", cfg.GRPCAddr, "mtls_enabled", cfg.MTLS.Enabled, "ca_persisted", cfg.CACertFile != "")

	ca, err := loadOrGenerateCA(cfg, log)
	if err != nil {
		return err
	}

	jwtSigner, err := jwtauth.New(cfg.JWTSigningKey)
	if err != nil {
		return err
	}

	srv := grpcserver.New(apikey.New(), jwtSigner, ca)

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
	authpb.RegisterAuthServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	metricsSrv := met.Server(cfg.MetricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		log.Info("auth gRPC server listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("auth metrics server listening", "addr", cfg.MetricsAddr)
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

// loadOrGenerateCA loads the runtime-issuance CA from
// AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE if configured, otherwise generates
// a fresh one in memory — see internal/servicecertca's package doc for
// why the generated path is ephemeral (ok for local/dev, a real
// deployment must set the file-backed path against a persisted,
// properly managed CA).
func loadOrGenerateCA(cfg *config.Config, log *slog.Logger) (*servicecertca.CA, error) {
	if cfg.CACertFile == "" {
		log.Warn("AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE not set: generating an ephemeral in-memory service-cert CA " +
			"— every certificate issued is only trusted by verifiers that fetch THIS run's CAPEM, and all of " +
			"them stop being trusted once this process restarts and generates a new root; see " +
			"services/auth/internal/servicecertca's package doc for the production TODO")
		return servicecertca.GenerateSelfSigned()
	}
	certPEM, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(cfg.CAKeyFile)
	if err != nil {
		return nil, err
	}
	return servicecertca.LoadFromPEM(certPEM, keyPEM)
}
