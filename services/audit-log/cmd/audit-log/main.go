// Command audit-log is the entry point for the append-only, hash-chained
// audit trail service (see proto/audit.proto, internal/store,
// docs/THREAT-MODEL.md). It is deliberately a separate deployable and a
// separate trust boundary from ledger-core: ledger-core mirrors its own
// ApplyEntry/RecordAgentDecision outcomes here on a best-effort basis,
// but cannot edit or delete anything audit-log has already written.
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
	auditpb "github.com/caelum0x/ration/pkg/pb/audit"
	"github.com/caelum0x/ration/pkg/telemetry"
	"github.com/caelum0x/ration/services/audit-log/internal/config"
	"github.com/caelum0x/ration/services/audit-log/internal/grpcserver"
	"github.com/caelum0x/ration/services/audit-log/internal/store"
	"google.golang.org/grpc"
)

// serviceName/serviceVersion tag every trace span and log line this
// process emits (see pkg/telemetry), matching the convention every
// other Go service in this project already follows (e.g.
// services/gateway/cmd/gateway/main.go).
const (
	serviceName    = "audit-log"
	serviceVersion = "0.1.0"
)

func main() {
	log := telemetry.NewLogger(serviceName)

	if err := run(log); err != nil {
		log.Error("audit-log exited with error", "error", err)
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
	log.Info("config loaded", "grpc_addr", cfg.GRPCAddr, "mtls_enabled", cfg.MTLS.Enabled)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(cfg.DataDir, "audit.log")

	st, err := store.Open(logPath)
	if err != nil {
		return err
	}
	defer st.Close()

	startupCheck := st.VerifyChain()
	if !startupCheck.Intact {
		// Refuse to serve a known-tampered trail rather than quietly
		// accepting new appends onto a broken chain.
		log.Error("audit log failed integrity check on startup", "detail", startupCheck.Detail, "first_broken_sequence", startupCheck.FirstBrokenSequence)
		return fmt.Errorf("audit-log: on-disk chain failed integrity check at sequence %d: %s", startupCheck.FirstBrokenSequence, startupCheck.Detail)
	}
	log.Info("audit log integrity check passed", "records", startupCheck.RecordsChecked)

	srv := grpcserver.New(st)

	serverOpt, err := cfg.MTLS.ServerOption()
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		serverOpt,
		telemetry.ServerOption(),
		grpc.ChainUnaryInterceptor(telemetry.UnaryServerInterceptor(log), met.UnaryServerInterceptor()),
	)
	auditpb.RegisterAuditLogServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	metricsSrv := met.Server(cfg.MetricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		log.Info("audit-log gRPC server listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("audit-log metrics server listening", "addr", cfg.MetricsAddr)
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
	_ = metricsSrv.Shutdown(shutdownCtx)

	shutdownDone := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		grpcServer.Stop()
	}
	return nil
}
