// Package config loads and validates webhook-ingestion's configuration
// from the environment. This is webhook-ingestion's share of the
// gateway's former internal/config (docs/adr/0004): just the webhook
// secret and HTTP listener address that belonged to
// internal/httpserver + internal/airwallex's webhook verification.
package config

import (
	"fmt"
	"os"

	"github.com/caelum0x/ration/pkg/mtls"
)

// Config holds every environment-derived setting webhook-ingestion needs.
type Config struct {
	AirwallexWebhookSecret string

	LedgerCoreAddr string
	GRPCAddr       string
	HTTPAddr       string
	MetricsAddr    string

	// MTLS configures mutual TLS for webhook-ingestion's own gRPC server
	// and for dialing ledger-core (see pkg/mtls, infra/dev-ca/). Disabled
	// by default. Deliberately does NOT cover the plain HTTP webhook
	// listener (HTTPAddr) — Airwallex's webhook deliveries are verified
	// by HMAC signature (internal/airwallex), not mTLS, matching how
	// Airwallex itself delivers webhooks in production.
	MTLS mtls.Config
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		AirwallexWebhookSecret: os.Getenv("AIRWALLEX_WEBHOOK_SECRET"),

		LedgerCoreAddr: getenvDefault("LEDGER_CORE_ADDR", "localhost:7001"),
		// Ports 7010-7013 are reserved for the section 1 split
		// (auth=7010, tenant-registry=7011, card-issuance=7012,
		// webhook-ingestion=7013), distinct from ledger-core (7001),
		// gateway (7002), audit-log (7003) and agent (no inbound port).
		GRPCAddr:    getenvDefault("WEBHOOK_INGESTION_GRPC_ADDR", "localhost:7013"),
		HTTPAddr:    getenvDefault("WEBHOOK_INGESTION_HTTP_ADDR", "localhost:8081"),
		MetricsAddr: getenvDefault("WEBHOOK_INGESTION_METRICS_ADDR", "localhost:9097"),
	}

	var err error
	cfg.MTLS, err = mtls.LoadFromEnv("WEBHOOK_INGESTION")
	if err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	// AirwallexWebhookSecret is deliberately NOT required at startup:
	// every other service in this project (AIRWALLEX_MODE, etc.) treats
	// mock-mode credentials as optional, and a webhook-ingestion
	// instance with no secret configured still starts and serves
	// StreamEvents — it will simply reject every delivery's signature
	// check (ErrMissingSecret, see internal/airwallex), which is the
	// correct fail-closed behavior for an unconfigured verifier, not a
	// reason to refuse to start the whole process.
	if c.LedgerCoreAddr == "" {
		return fmt.Errorf("config: LEDGER_CORE_ADDR must not be empty")
	}
	if c.GRPCAddr == "" {
		return fmt.Errorf("config: WEBHOOK_INGESTION_GRPC_ADDR must not be empty")
	}
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: WEBHOOK_INGESTION_HTTP_ADDR must not be empty")
	}
	return nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
