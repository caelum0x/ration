// Package config loads and validates card-issuance's configuration from
// the environment. It fails fast (returns an error from Load, never
// panics) when AIRWALLEX_MODE=live and a required live-mode credential
// is missing, since a half-configured live client is worse than
// refusing to start.
//
// This is card-issuance's share of the gateway's former
// internal/config (docs/adr/0004): the Airwallex and bridge-policy
// settings that belonged to internal/card + internal/bridge move here
// verbatim; webhook-secret/HTTP-listener settings move to
// services/webhook-ingestion instead.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/caelum0x/ration/pkg/mtls"
)

// Mode is the Airwallex integration mode.
type Mode string

const (
	ModeMock Mode = "mock"
	ModeLive Mode = "live"
)

// Config holds every environment-derived setting card-issuance needs.
type Config struct {
	AirwallexMode     Mode
	AirwallexClientID string
	AirwallexAPIKey   string
	AirwallexBaseURL  string

	LedgerCoreAddr string
	// AuthAddr is where card-issuance mints its own operator-scoped
	// credential at startup (see internal/authclient.MintOperatorJWT)
	// to present on every direct ledger-core call (ApplyEntry for
	// bridge-funding HOLD/RELEASE and card-limit DEBIT/RELEASE) — see
	// services/ledger-core/src/tenantauth.rs, docs/THREAT-MODEL.md
	// item 1's "the biggest one" sub-gap.
	AuthAddr    string
	GRPCAddr    string
	MetricsAddr string

	BridgeMaxFraction        float64
	BridgeMinGrantMinorUnits int64
	BridgeVelocityWindow     time.Duration

	// MTLS configures mutual TLS for card-issuance's own gRPC server and
	// for dialing ledger-core (see pkg/mtls, infra/dev-ca/). Disabled by
	// default.
	MTLS mtls.Config
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		AirwallexMode:     Mode(getenvDefault("AIRWALLEX_MODE", string(ModeMock))),
		AirwallexClientID: os.Getenv("AIRWALLEX_CLIENT_ID"),
		AirwallexAPIKey:   os.Getenv("AIRWALLEX_API_KEY"),
		AirwallexBaseURL:  getenvDefault("AIRWALLEX_BASE_URL", "https://api-demo.airwallex.com"),

		LedgerCoreAddr: getenvDefault("LEDGER_CORE_ADDR", "localhost:7001"),
		AuthAddr:       getenvDefault("AUTH_GRPC_ADDR", "localhost:7010"),
		// Ports 7010-7013 are reserved for the section 1 split
		// (auth=7010, tenant-registry=7011, card-issuance=7012,
		// webhook-ingestion=7013), distinct from ledger-core (7001),
		// gateway (7002), audit-log (7003) and agent (no inbound port).
		GRPCAddr:    getenvDefault("CARD_ISSUANCE_GRPC_ADDR", "localhost:7012"),
		MetricsAddr: getenvDefault("CARD_ISSUANCE_METRICS_ADDR", "localhost:9096"),
	}

	var err error
	cfg.BridgeMaxFraction, err = getenvFloatDefault("RATION_BRIDGE_MAX_FRACTION", 0.6)
	if err != nil {
		return nil, err
	}
	cfg.BridgeMinGrantMinorUnits, err = getenvInt64Default("RATION_BRIDGE_MIN_GRANT_MINOR_UNITS", 500)
	if err != nil {
		return nil, err
	}
	velocityHours, err := getenvInt64Default("RATION_BRIDGE_VELOCITY_WINDOW_HOURS", 24)
	if err != nil {
		return nil, err
	}
	cfg.BridgeVelocityWindow = time.Duration(velocityHours) * time.Hour

	cfg.MTLS, err = mtls.LoadFromEnv("CARD_ISSUANCE")
	if err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.AirwallexMode {
	case ModeMock:
		// No credentials required; the mock client is fully self-contained.
	case ModeLive:
		missing := []string{}
		if c.AirwallexClientID == "" {
			missing = append(missing, "AIRWALLEX_CLIENT_ID")
		}
		if c.AirwallexAPIKey == "" {
			missing = append(missing, "AIRWALLEX_API_KEY")
		}
		if c.AirwallexBaseURL == "" {
			missing = append(missing, "AIRWALLEX_BASE_URL")
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: AIRWALLEX_MODE=live requires env vars: %v", missing)
		}
	default:
		return fmt.Errorf("config: invalid AIRWALLEX_MODE %q (want %q or %q)", c.AirwallexMode, ModeMock, ModeLive)
	}

	if c.BridgeMaxFraction <= 0 || c.BridgeMaxFraction > 1 {
		return fmt.Errorf("config: RATION_BRIDGE_MAX_FRACTION must be in (0, 1], got %v", c.BridgeMaxFraction)
	}
	if c.BridgeMinGrantMinorUnits < 0 {
		return fmt.Errorf("config: RATION_BRIDGE_MIN_GRANT_MINOR_UNITS must be >= 0, got %d", c.BridgeMinGrantMinorUnits)
	}
	if c.BridgeVelocityWindow <= 0 {
		return fmt.Errorf("config: RATION_BRIDGE_VELOCITY_WINDOW_HOURS must be > 0")
	}
	if c.LedgerCoreAddr == "" {
		return fmt.Errorf("config: LEDGER_CORE_ADDR must not be empty")
	}
	if c.AuthAddr == "" {
		return fmt.Errorf("config: AUTH_GRPC_ADDR must not be empty")
	}
	if c.GRPCAddr == "" {
		return fmt.Errorf("config: CARD_ISSUANCE_GRPC_ADDR must not be empty")
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
