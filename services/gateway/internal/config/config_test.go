package config

import "testing"

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"AIRWALLEX_MODE", "AIRWALLEX_CLIENT_ID", "AIRWALLEX_API_KEY",
		"AIRWALLEX_BASE_URL", "AIRWALLEX_WEBHOOK_SECRET", "LEDGER_CORE_ADDR",
		"GATEWAY_GRPC_ADDR", "GATEWAY_HTTP_ADDR", "RATION_DATA_DIR",
		"RATION_BRIDGE_MAX_FRACTION", "RATION_BRIDGE_MIN_GRANT_MINOR_UNITS",
		"RATION_BRIDGE_VELOCITY_WINDOW_HOURS",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestLoad_MockModeDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.AirwallexMode != ModeMock {
		t.Errorf("AirwallexMode = %q, want %q", cfg.AirwallexMode, ModeMock)
	}
	if cfg.BridgeMaxFraction != 0.6 {
		t.Errorf("BridgeMaxFraction = %v, want 0.6", cfg.BridgeMaxFraction)
	}
	if cfg.BridgeMinGrantMinorUnits != 500 {
		t.Errorf("BridgeMinGrantMinorUnits = %v, want 500", cfg.BridgeMinGrantMinorUnits)
	}
}

func TestLoad_LiveModeRequiresCredentials(t *testing.T) {
	clearEnv(t)
	t.Setenv("AIRWALLEX_MODE", "live")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for live mode missing credentials, got nil")
	}
}

func TestLoad_LiveModeWithCredentials(t *testing.T) {
	clearEnv(t)
	t.Setenv("AIRWALLEX_MODE", "live")
	t.Setenv("AIRWALLEX_CLIENT_ID", "client-123")
	t.Setenv("AIRWALLEX_API_KEY", "key-123")
	t.Setenv("AIRWALLEX_WEBHOOK_SECRET", "secret-123")
	t.Setenv("AIRWALLEX_BASE_URL", "https://api-demo.airwallex.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.AirwallexMode != ModeLive {
		t.Errorf("AirwallexMode = %q, want %q", cfg.AirwallexMode, ModeLive)
	}
}

func TestLoad_InvalidMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("AIRWALLEX_MODE", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid mode, got nil")
	}
}

func TestLoad_InvalidBridgeFraction(t *testing.T) {
	clearEnv(t)
	t.Setenv("RATION_BRIDGE_MAX_FRACTION", "1.5")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for out-of-range bridge fraction, got nil")
	}
}
