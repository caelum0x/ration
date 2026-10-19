// Package config loads and validates gateway configuration from the
// environment. It fails fast (returns an error from Load, never panics)
// when AIRWALLEX_MODE=live and a required live-mode credential is
// missing, since a half-configured live client is worse than refusing to
// start.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Mode is the Airwallex integration mode.
type Mode string

const (
	ModeMock Mode = "mock"
	ModeLive Mode = "live"
)

// Config holds every environment-derived setting the gateway needs.
type Config struct {
	AirwallexMode          Mode
	AirwallexClientID      string
	AirwallexAPIKey        string
	AirwallexBaseURL       string
	AirwallexWebhookSecret string

	LedgerCoreAddr  string
	GatewayGRPCAddr string
	GatewayHTTPAddr string
	DataDir         string

	BridgeMaxFraction        float64
	BridgeMinGrantMinorUnits int64
	BridgeVelocityWindow     time.Duration
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		AirwallexMode:          Mode(getenvDefault("AIRWALLEX_MODE", string(ModeMock))),
		AirwallexClientID:      os.Getenv("AIRWALLEX_CLIENT_ID"),
		AirwallexAPIKey:        os.Getenv("AIRWALLEX_API_KEY"),
		AirwallexBaseURL:       getenvDefault("AIRWALLEX_BASE_URL", "https://api-demo.airwallex.com"),
		AirwallexWebhookSecret: os.Getenv("AIRWALLEX_WEBHOOK_SECRET"),

		LedgerCoreAddr:  getenvDefault("LEDGER_CORE_ADDR", "localhost:7001"),
		GatewayGRPCAddr: getenvDefault("GATEWAY_GRPC_ADDR", "localhost:7002"),
		GatewayHTTPAddr: getenvDefault("GATEWAY_HTTP_ADDR", "localhost:8080"),
		DataDir:         getenvDefault("RATION_DATA_DIR", "./data"),
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
		if c.AirwallexWebhookSecret == "" {
			missing = append(missing, "AIRWALLEX_WEBHOOK_SECRET")
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
