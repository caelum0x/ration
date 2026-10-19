// Package config loads and validates auth's configuration from the
// environment. Unlike every other mock/live toggle in this project,
// AUTH_JWT_SIGNING_KEY has no default: it is a secret that lets whoever
// holds it mint valid bearer tokens for any subject/tenant, so Load
// fails fast rather than falling back to a hardcoded key, per this
// project's standing security rule (never hardcode secrets, docs/
// SCALE-EXPANSION.md's production bar, docs/THREAT-MODEL.md).
package config

import (
	"fmt"
	"os"

	"github.com/caelum0x/ration/pkg/mtls"
)

// Config holds every environment-derived setting auth needs.
type Config struct {
	JWTSigningKey []byte

	// CACertFile/CAKeyFile optionally load a persisted runtime-issuance
	// CA (internal/servicecertca) from disk. When either is empty, auth
	// generates a fresh in-memory CA at startup — see
	// internal/servicecertca's package doc for why that's ephemeral and
	// the production TODO it leaves.
	CACertFile string
	CAKeyFile  string

	GRPCAddr    string
	MetricsAddr string

	// MTLS configures mutual TLS for auth's own gRPC server. auth has no
	// outbound gRPC dependency of its own (ledger-core is not on its
	// call path), so only the server side applies.
	MTLS mtls.Config
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		JWTSigningKey: []byte(os.Getenv("AUTH_JWT_SIGNING_KEY")),
		CACertFile:    os.Getenv("AUTH_CA_CERT_FILE"),
		CAKeyFile:     os.Getenv("AUTH_CA_KEY_FILE"),
		// Ports 7010-7013 are reserved for the section 1 split
		// (auth=7010, tenant-registry=7011, card-issuance=7012,
		// webhook-ingestion=7013), distinct from ledger-core (7001),
		// gateway (7002), audit-log (7003) and agent (no inbound port).
		GRPCAddr:    getenvDefault("AUTH_GRPC_ADDR", "localhost:7010"),
		MetricsAddr: getenvDefault("AUTH_METRICS_ADDR", "localhost:9098"),
	}

	var err error
	cfg.MTLS, err = mtls.LoadFromEnv("AUTH")
	if err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.JWTSigningKey) == 0 {
		return fmt.Errorf("config: AUTH_JWT_SIGNING_KEY must be set (no default — see this package's doc comment)")
	}
	if len(c.JWTSigningKey) < 16 {
		return fmt.Errorf("config: AUTH_JWT_SIGNING_KEY must be at least 16 bytes, got %d", len(c.JWTSigningKey))
	}
	if (c.CACertFile == "") != (c.CAKeyFile == "") {
		return fmt.Errorf("config: AUTH_CA_CERT_FILE and AUTH_CA_KEY_FILE must both be set or both be empty")
	}
	if c.GRPCAddr == "" {
		return fmt.Errorf("config: AUTH_GRPC_ADDR must not be empty")
	}
	return nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
