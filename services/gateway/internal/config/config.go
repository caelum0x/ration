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
	AuthAddr             string

	GatewayGRPCAddr string
	MetricsAddr     string

	// GatewayGRPCDialAddr is the address internal/httpapi's REST facade
	// dials to reach this same process's own gRPC listener
	// (GatewayGRPCAddr above) — separate from GatewayGRPCAddr because
	// that is a *bind* address (e.g. "0.0.0.0:7002" in docker-compose)
	// which is not always a valid *dial* target for the same process on
	// every platform, whereas this is always a loopback address this
	// process can reach itself at. Defaults to the same port
	// GatewayGRPCAddr defaults to.
	GatewayGRPCDialAddr string

	// HTTPAddr is where internal/httpapi's REST/JSON facade
	// (docs/SCALE-EXPANSION.md section 7, docs/adr/0003) listens.
	HTTPAddr string

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

	// RequireTenantAuth gates internal/tenantauth's interceptor
	// (docs/THREAT-MODEL.md's "Known gaps" item 1): off by default, the
	// same opt-in pattern as MTLS.Enabled above, since the demo/CLI/TUI
	// flows don't present a credential on every call yet (see
	// internal/tenantauth's package doc for the precise follow-up TODO).
	RequireTenantAuth bool
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
		AuthAddr:             getenvDefault("AUTH_ADDR", "localhost:7010"),

		GatewayGRPCAddr: getenvDefault("GATEWAY_GRPC_ADDR", "localhost:7002"),
		MetricsAddr:     getenvDefault("GATEWAY_METRICS_ADDR", "localhost:9091"),

		GatewayGRPCDialAddr: getenvDefault("GATEWAY_GRPC_DIAL_ADDR", "localhost:7002"),
		HTTPAddr:            getenvDefault("GATEWAY_HTTP_ADDR", "localhost:8082"),
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

	cfg.RequireTenantAuth, err = getenvBoolDefault("RATION_GATEWAY_REQUIRE_TENANT_AUTH", false)
	if err != nil {
		return nil, err
	}

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
	if c.AuthAddr == "" {
		return fmt.Errorf("config: AUTH_ADDR must not be empty")
	}
	if c.GatewayGRPCAddr == "" {
		return fmt.Errorf("config: GATEWAY_GRPC_ADDR must not be empty")
	}
	if c.GatewayGRPCDialAddr == "" {
		return fmt.Errorf("config: GATEWAY_GRPC_DIAL_ADDR must not be empty")
	}
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: GATEWAY_HTTP_ADDR must not be empty")
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

func getenvBoolDefault(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: invalid %s=%q: %w", key, v, err)
	}
	return b, nil
}
