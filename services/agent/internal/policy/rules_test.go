package policy

import (
	"testing"
)

func testConfig() Config {
	return Config{
		BridgeMaxFraction:   0.6,
		MinGrantMinorUnits:  500,
		VelocityWindowHours: 24,
	}
}

func TestComputeBridgeEnvelope_FullGrant(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        100000,
		ExpectedDepositMinorUnits: 60000,
		RequestedMinorUnits:       30000, // under both headroom and the 60% cap (36000)
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionFullGrant {
		t.Fatalf("decision = %q, want %q (reason: %s)", env.Decision, DecisionFullGrant, env.Reason)
	}
	if env.MaxGrantMinorUnits != 30000 {
		t.Fatalf("max grant = %d, want 30000", env.MaxGrantMinorUnits)
	}
	if env.Reason == "" {
		t.Fatal("expected a non-empty reason")
	}
}

func TestComputeBridgeEnvelope_PartialGrant_CappedByBridgeFraction(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        1_000_000, // plenty of headroom
		ExpectedDepositMinorUnits: 10000,     // 60% cap = 6000
		RequestedMinorUnits:       10000,     // asks for the full deposit amount
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionPartialGrant {
		t.Fatalf("decision = %q, want %q (reason: %s)", env.Decision, DecisionPartialGrant, env.Reason)
	}
	if env.MaxGrantMinorUnits != 6000 {
		t.Fatalf("max grant = %d, want 6000", env.MaxGrantMinorUnits)
	}
	if env.BridgeCapMinorUnits != 6000 {
		t.Fatalf("bridge cap = %d, want 6000", env.BridgeCapMinorUnits)
	}
}

func TestComputeBridgeEnvelope_PartialGrant_CappedByReserveFloorHeadroom(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        2000, // the binding constraint
		ExpectedDepositMinorUnits: 100000,
		RequestedMinorUnits:       50000,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionPartialGrant {
		t.Fatalf("decision = %q, want %q (reason: %s)", env.Decision, DecisionPartialGrant, env.Reason)
	}
	if env.MaxGrantMinorUnits != 2000 {
		t.Fatalf("max grant = %d, want 2000 (reserve floor headroom should bind)", env.MaxGrantMinorUnits)
	}
}

func TestComputeBridgeEnvelope_DeclinedBelowMinimumUsefulGrant(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        1_000_000,
		ExpectedDepositMinorUnits: 400, // 60% cap = 240, below the 500 minimum
		RequestedMinorUnits:       400,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionDeclined {
		t.Fatalf("decision = %q, want %q (reason: %s)", env.Decision, DecisionDeclined, env.Reason)
	}
	if env.MaxGrantMinorUnits != 0 {
		t.Fatalf("max grant = %d, want 0", env.MaxGrantMinorUnits)
	}
	if env.Reason == "" {
		t.Fatal("expected a reason explaining the decline")
	}
}

func TestComputeBridgeEnvelope_GrantEqualToMinimumIsNotDeclined(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        1_000_000,
		ExpectedDepositMinorUnits: 834, // 60% of 834 = 500.4 -> truncates to 500, equal to minimum
		RequestedMinorUnits:       834,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision == DecisionDeclined {
		t.Fatalf("decision = %q, want a grant decision when the computed amount equals the minimum (reason: %s)", env.Decision, env.Reason)
	}
	if env.MaxGrantMinorUnits != 500 {
		t.Fatalf("max grant = %d, want 500", env.MaxGrantMinorUnits)
	}
}

func TestComputeBridgeEnvelope_DeclinedNoHeadroom(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        0,
		ExpectedDepositMinorUnits: 100000,
		RequestedMinorUnits:       50000,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionDeclined {
		t.Fatalf("decision = %q, want %q", env.Decision, DecisionDeclined)
	}
	if env.MaxGrantMinorUnits != 0 {
		t.Fatalf("max grant = %d, want 0", env.MaxGrantMinorUnits)
	}
}

func TestComputeBridgeEnvelope_DeclinedNegativeHeadroom(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        -5000, // available balance below the reserve floor
		ExpectedDepositMinorUnits: 100000,
		RequestedMinorUnits:       50000,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionDeclined {
		t.Fatalf("decision = %q, want %q", env.Decision, DecisionDeclined)
	}
	if env.HeadroomMinorUnits != 0 {
		t.Fatalf("headroom should clamp to 0, got %d", env.HeadroomMinorUnits)
	}
}

func TestComputeBridgeEnvelope_VelocityCapBlocksEvenWithHeadroom(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:         1_000_000,
		ExpectedDepositMinorUnits:  100000,
		RequestedMinorUnits:        50000,
		PriorGrantOpenWithinWindow: true,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionDeclined {
		t.Fatalf("decision = %q, want %q (velocity cap should block)", env.Decision, DecisionDeclined)
	}
	if env.MaxGrantMinorUnits != 0 {
		t.Fatalf("max grant = %d, want 0", env.MaxGrantMinorUnits)
	}
	if env.Reason == "" || env.BridgeCapMinorUnits == 0 {
		// Bridge cap should still be computed for the audit snapshot even
		// though the grant is blocked.
		t.Fatalf("expected a populated reason and a non-zero bridge cap for audit purposes, got reason=%q cap=%d", env.Reason, env.BridgeCapMinorUnits)
	}
}

func TestComputeBridgeEnvelope_DeclinedNonPositiveRequest(t *testing.T) {
	cfg := testConfig()

	for _, requested := range []int64{0, -100} {
		in := EnvelopeInput{
			HeadroomMinorUnits:        1_000_000,
			ExpectedDepositMinorUnits: 100000,
			RequestedMinorUnits:       requested,
		}

		env := ComputeBridgeEnvelope(cfg, in)

		if env.Decision != DecisionDeclined {
			t.Fatalf("requested=%d: decision = %q, want %q", requested, env.Decision, DecisionDeclined)
		}
	}
}

func TestComputeBridgeEnvelope_DeclinedZeroExpectedDeposit(t *testing.T) {
	cfg := testConfig()
	in := EnvelopeInput{
		HeadroomMinorUnits:        1_000_000,
		ExpectedDepositMinorUnits: 0,
		RequestedMinorUnits:       1000,
	}

	env := ComputeBridgeEnvelope(cfg, in)

	if env.Decision != DecisionDeclined {
		t.Fatalf("decision = %q, want %q (no expected deposit means a zero bridge cap)", env.Decision, DecisionDeclined)
	}
	if env.BridgeCapMinorUnits != 0 {
		t.Fatalf("bridge cap = %d, want 0", env.BridgeCapMinorUnits)
	}
}

func TestLoadConfigFromEnv_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("RATION_BRIDGE_MAX_FRACTION", "")
	t.Setenv("RATION_BRIDGE_MIN_GRANT_MINOR_UNITS", "")
	t.Setenv("RATION_BRIDGE_VELOCITY_WINDOW_HOURS", "")

	cfg := LoadConfigFromEnv()
	want := DefaultConfig()

	if cfg != want {
		t.Fatalf("cfg = %+v, want defaults %+v", cfg, want)
	}
}

func TestLoadConfigFromEnv_OverridesFromEnv(t *testing.T) {
	t.Setenv("RATION_BRIDGE_MAX_FRACTION", "0.5")
	t.Setenv("RATION_BRIDGE_MIN_GRANT_MINOR_UNITS", "1000")
	t.Setenv("RATION_BRIDGE_VELOCITY_WINDOW_HOURS", "12")

	cfg := LoadConfigFromEnv()

	if cfg.BridgeMaxFraction != 0.5 {
		t.Errorf("BridgeMaxFraction = %v, want 0.5", cfg.BridgeMaxFraction)
	}
	if cfg.MinGrantMinorUnits != 1000 {
		t.Errorf("MinGrantMinorUnits = %v, want 1000", cfg.MinGrantMinorUnits)
	}
	if cfg.VelocityWindowHours != 12 {
		t.Errorf("VelocityWindowHours = %v, want 12", cfg.VelocityWindowHours)
	}
}

func TestLoadConfigFromEnv_IgnoresInvalidValues(t *testing.T) {
	t.Setenv("RATION_BRIDGE_MAX_FRACTION", "not-a-number")
	t.Setenv("RATION_BRIDGE_MIN_GRANT_MINOR_UNITS", "not-a-number")
	t.Setenv("RATION_BRIDGE_VELOCITY_WINDOW_HOURS", "not-a-number")

	cfg := LoadConfigFromEnv()
	want := DefaultConfig()

	if cfg != want {
		t.Fatalf("cfg = %+v, want defaults %+v on invalid input", cfg, want)
	}
}
