// Package config loads audit-log's configuration from the environment,
// following the same fail-fast-on-misconfiguration convention as
// services/gateway/internal/config.
package config

import (
	"fmt"
	"os"

	"github.com/caelum0x/ration/pkg/mtls"
)

// Config holds every environment-derived setting audit-log needs.
type Config struct {
	GRPCAddr    string
	DataDir     string
	MetricsAddr string

	// MTLS configures mutual TLS for audit-log's own gRPC server (see
	// pkg/mtls, infra/dev-ca/). Disabled by default.
	MTLS mtls.Config
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		GRPCAddr: getenvDefault("AUDIT_LOG_GRPC_ADDR", "localhost:7003"),
		DataDir:  getenvDefault("RATION_AUDIT_LOG_DATA_DIR", "./data"),
		// 9092 slots between gateway's 9091 and agent's 9093 (see
		// services/gateway/internal/config, services/agent/cmd/agent) —
		// every service's Prometheus /metrics endpoint gets its own port
		// so a single docker-compose/Prometheus scrape config can list
		// them all without collisions.
		MetricsAddr: getenvDefault("AUDIT_LOG_METRICS_ADDR", "localhost:9092"),
	}

	var err error
	cfg.MTLS, err = mtls.LoadFromEnv("AUDIT_LOG")
	if err != nil {
		return nil, err
	}

	if cfg.GRPCAddr == "" {
		return nil, fmt.Errorf("config: AUDIT_LOG_GRPC_ADDR must not be empty")
	}
	return cfg, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
