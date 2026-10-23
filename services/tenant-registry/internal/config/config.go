// Package config loads tenant-registry's configuration from the
// environment, following the same fail-fast-on-misconfiguration
// convention as every other Go service in this project (see
// services/gateway/internal/config, services/audit-log/internal/config).
package config

import (
	"fmt"
	"os"

	"github.com/caelum0x/ration/pkg/mtls"
)

// Config holds every environment-derived setting tenant-registry needs.
type Config struct {
	LedgerCoreAddr string
	// AuthAddr is where tenant-registry mints its own operator-scoped
	// credential at startup (see internal/authclient.MintOperatorJWT)
	// to present on every direct ledger-core call (CreateTenant/
	// GetTenant/ListTenants — operator-only RPCs on ledger-core's own
	// side too, see services/ledger-core/src/tenantauth.rs) —
	// docs/THREAT-MODEL.md item 1's "the biggest one" sub-gap.
	AuthAddr    string
	GRPCAddr    string
	MetricsAddr string

	// MTLS configures mutual TLS for tenant-registry's own gRPC server
	// and for dialing ledger-core (see pkg/mtls, infra/dev-ca/).
	// Disabled by default.
	MTLS mtls.Config
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		LedgerCoreAddr: getenvDefault("LEDGER_CORE_ADDR", "localhost:7001"),
		AuthAddr:       getenvDefault("AUTH_GRPC_ADDR", "localhost:7010"),
		GRPCAddr:       getenvDefault("TENANT_REGISTRY_GRPC_ADDR", "localhost:7011"),
		// Ports 7010-7013 are reserved for the section 1 split
		// (auth=7010, tenant-registry=7011, card-issuance=7012,
		// webhook-ingestion=7013), distinct from ledger-core (7001),
		// gateway (7002), audit-log (7003) and agent (no inbound port).
		MetricsAddr: getenvDefault("TENANT_REGISTRY_METRICS_ADDR", "localhost:9095"),
	}

	var err error
	cfg.MTLS, err = mtls.LoadFromEnv("TENANT_REGISTRY")
	if err != nil {
		return nil, err
	}

	if cfg.LedgerCoreAddr == "" {
		return nil, fmt.Errorf("config: LEDGER_CORE_ADDR must not be empty")
	}
	if cfg.AuthAddr == "" {
		return nil, fmt.Errorf("config: AUTH_GRPC_ADDR must not be empty")
	}
	if cfg.GRPCAddr == "" {
		return nil, fmt.Errorf("config: TENANT_REGISTRY_GRPC_ADDR must not be empty")
	}
	return cfg, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
