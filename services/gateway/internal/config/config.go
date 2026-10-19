// Package config loads and validates gateway configuration from the
// environment. The gateway is a thin BFF (docs/SCALE-EXPANSION.md
// section 1, docs/adr/0004): it no longer owns Airwallex credentials or
// bridge-funding policy (those moved to services/card-issuance) or a
// webhook secret/listener (moved to services/webhook-ingestion) — this
// package is left with exactly what a pure fan-out layer needs: where
// to dial its four backing services, its own listen addresses, mTLS and
// rate-limit settings.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/caelum0x/ration/pkg/mtls"
)

// Config holds every environment-derived setting the gateway needs.
type Config struct {
	LedgerCoreAddr       string
	CardIssuanceAddr     string
	WebhookIngestionAddr string

	GatewayGRPCAddr string
	MetricsAddr     string

	// MTLS configures mutual TLS for the gateway's own gRPC server and
	// for dialing its backing services (see pkg/mtls, infra/dev-ca/).
	// Disabled by default — see GATEWAY_MTLS_ENABLED in pkg/mtls's doc
	// comment.
	MTLS mtls.Config

	// RateLimit* tunes the per-tenant token bucket (see
	// internal/ratelimit) in front of every RPC on the gateway's
	// public gRPC surface.
	RateLimitRPS           float64
	RateLimitBurst         float64
	RateLimitSweepInterval time.Duration
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		LedgerCoreAddr: getenvDefault("LEDGER_CORE_ADDR", "localhost:7001"),
		// Ports 7010-7013 are reserved for the section 1 split
		// (auth=7010, tenant-registry=7011, card-issuance=7012,
		// webhook-ingestion=7013) — see each of those services' own
		// internal/config for the same comment.
		CardIssuanceAddr:     getenvDefault("CARD_ISSUANCE_ADDR", "localhost:7012"),
		WebhookIngestionAddr: getenvDefault("WEBHOOK_INGESTION_ADDR", "localhost:7013"),

		GatewayGRPCAddr: getenvDefault("GATEWAY_GRPC_ADDR", "localhost:7002"),
		MetricsAddr:     getenvDefault("GATEWAY_METRICS_ADDR", "localhost:9091"),
	}

	var err error
	cfg.MTLS, err = mtls.LoadFromEnv("GATEWAY")
	if err != nil {
		return nil, err
	}

	cfg.RateLimitRPS, err = getenvFloatDefault("RATION_RATELIMIT_RPS", 20)
	if err != nil {
		return nil, err
	}
	cfg.RateLimitBurst, err = getenvFloatDefault("RATION_RATELIMIT_BURST", 40)
	if err != nil {
		return nil, err
	}
	sweepMinutes, err := getenvInt64Default("RATION_RATELIMIT_SWEEP_INTERVAL_MINUTES", 10)
	if err != nil {
		return nil, err
	}
	cfg.RateLimitSweepInterval = time.Duration(sweepMinutes) * time.Minute

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.LedgerCoreAddr == "" {
		return fmt.Errorf("config: LEDGER_CORE_ADDR must not be empty")
	}
	if c.CardIssuanceAddr == "" {
		return fmt.Errorf("config: CARD_ISSUANCE_ADDR must not be empty")
	}
	if c.WebhookIngestionAddr == "" {
		return fmt.Errorf("config: WEBHOOK_INGESTION_ADDR must not be empty")
	}
	if c.GatewayGRPCAddr == "" {
		return fmt.Errorf("config: GATEWAY_GRPC_ADDR must not be empty")
	}
	return nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvFloatDefault(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: invalid %s=%q: %w", key, v, err)
	}
	return f, nil
}

func getenvInt64Default(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: invalid %s=%q: %w", key, v, err)
	}
	return i, nil
}
