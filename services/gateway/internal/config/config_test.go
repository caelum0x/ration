package config

import "testing"

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"LEDGER_CORE_ADDR", "CARD_ISSUANCE_ADDR", "WEBHOOK_INGESTION_ADDR",
		"GATEWAY_GRPC_ADDR", "GATEWAY_METRICS_ADDR",
		"RATION_RATELIMIT_RPS", "RATION_RATELIMIT_BURST",
		"RATION_RATELIMIT_SWEEP_INTERVAL_MINUTES",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.LedgerCoreAddr != "localhost:7001" {
		t.Errorf("LedgerCoreAddr = %q, want localhost:7001", cfg.LedgerCoreAddr)
	}
	if cfg.CardIssuanceAddr != "localhost:7012" {
		t.Errorf("CardIssuanceAddr = %q, want localhost:7012", cfg.CardIssuanceAddr)
	}
	if cfg.WebhookIngestionAddr != "localhost:7013" {
		t.Errorf("WebhookIngestionAddr = %q, want localhost:7013", cfg.WebhookIngestionAddr)
	}
	if cfg.RateLimitRPS != 20 {
		t.Errorf("RateLimitRPS = %v, want 20", cfg.RateLimitRPS)
	}
	if cfg.RateLimitBurst != 40 {
		t.Errorf("RateLimitBurst = %v, want 40", cfg.RateLimitBurst)
	}
}

func TestLoad_OverridesFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("CARD_ISSUANCE_ADDR", "card-issuance:7012")
	t.Setenv("WEBHOOK_INGESTION_ADDR", "webhook-ingestion:7013")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.CardIssuanceAddr != "card-issuance:7012" {
		t.Errorf("CardIssuanceAddr = %q, want card-issuance:7012", cfg.CardIssuanceAddr)
	}
	if cfg.WebhookIngestionAddr != "webhook-ingestion:7013" {
		t.Errorf("WebhookIngestionAddr = %q, want webhook-ingestion:7013", cfg.WebhookIngestionAddr)
	}
}

func TestLoad_InvalidRateLimitRPS(t *testing.T) {
	clearEnv(t)
	t.Setenv("RATION_RATELIMIT_RPS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid RATION_RATELIMIT_RPS, got nil")
	}
}
