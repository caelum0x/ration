package policy

import (
	"math"
	"testing"
)

// FuzzComputeBridgeEnvelope exercises ComputeBridgeEnvelope — the pure,
// deterministic hard-rule function docs/SCALE-EXPANSION.md section 5
// calls out by name — against randomly generated tenant headrooms,
// expected late-deposit amounts, requested amounts and velocity states.
//
// ComputeBridgeEnvelope is deliberately the only thing being fuzzed here
// (not, say, bridge.Service.RationBridgeFunding): it is a pure function
// of its inputs with no I/O, no clock, and no mocking required, so the
// fuzzer's random inputs translate directly into random
// tenant-balance/reserve-floor/deposit-amount combinations rather than
// mostly exercising a fake gRPC client. The property under test is the
// one stated in the spec and in rules.go's own doc comment: a bridge
// grant may never exceed the tenant's reserve-floor headroom, no matter
// what garbage/edge-case numbers (including negative headroom, zero or
// negative requests, and int64-extreme deposit amounts) are thrown at
// it — and the function must never panic while deciding that.
func FuzzComputeBridgeEnvelope(f *testing.F) {
	cfg := DefaultConfig()

	// Seed corpus: the representative cases already covered by
	// rules_test.go's table tests, so the fuzzer starts from known
	// interesting boundaries (exact minimum-grant floor, exact bridge
	// cap, negative headroom, velocity-blocked, non-positive request)
	// rather than purely random bytes.
	seeds := []EnvelopeInput{
		{HeadroomMinorUnits: 100000, ExpectedDepositMinorUnits: 60000, RequestedMinorUnits: 30000},
		{HeadroomMinorUnits: 1_000_000, ExpectedDepositMinorUnits: 10000, RequestedMinorUnits: 10000},
		{HeadroomMinorUnits: 2000, ExpectedDepositMinorUnits: 100000, RequestedMinorUnits: 50000},
		{HeadroomMinorUnits: 1_000_000, ExpectedDepositMinorUnits: 400, RequestedMinorUnits: 400},
		{HeadroomMinorUnits: 1_000_000, ExpectedDepositMinorUnits: 834, RequestedMinorUnits: 834},
		{HeadroomMinorUnits: 0, ExpectedDepositMinorUnits: 100000, RequestedMinorUnits: 50000},
		{HeadroomMinorUnits: -5000, ExpectedDepositMinorUnits: 100000, RequestedMinorUnits: 50000},
		{HeadroomMinorUnits: 1_000_000, ExpectedDepositMinorUnits: 100000, RequestedMinorUnits: 50000, PriorGrantOpenWithinWindow: true},
		{HeadroomMinorUnits: 1_000_000, ExpectedDepositMinorUnits: 100000, RequestedMinorUnits: 0},
		{HeadroomMinorUnits: 1_000_000, ExpectedDepositMinorUnits: 100000, RequestedMinorUnits: -100},
		{HeadroomMinorUnits: math.MaxInt64, ExpectedDepositMinorUnits: math.MaxInt64, RequestedMinorUnits: math.MaxInt64},
		{HeadroomMinorUnits: math.MinInt64, ExpectedDepositMinorUnits: math.MinInt64, RequestedMinorUnits: math.MinInt64},
	}
	for _, s := range seeds {
		f.Add(s.HeadroomMinorUnits, s.ExpectedDepositMinorUnits, s.RequestedMinorUnits, s.PriorGrantOpenWithinWindow)
	}

	f.Fuzz(func(t *testing.T, headroom, expectedDeposit, requested int64, priorOpen bool) {
		in := EnvelopeInput{
			HeadroomMinorUnits:         headroom,
			ExpectedDepositMinorUnits:  expectedDeposit,
			RequestedMinorUnits:        requested,
			PriorGrantOpenWithinWindow: priorOpen,
		}

		// Must never panic for any input — a hard rule that gates real
		// money movement crashing is itself a production incident.
		env := ComputeBridgeEnvelope(cfg, in)

		// The invariant this harness exists to prove: the computed
		// grant can never exceed the tenant's headroom above its
		// reserve floor (clamped at 0 — negative headroom grants
		// nothing, never a negative amount either).
		effectiveHeadroom := headroom
		if effectiveHeadroom < 0 {
			effectiveHeadroom = 0
		}
		if env.MaxGrantMinorUnits > effectiveHeadroom {
			t.Fatalf(
				"MaxGrantMinorUnits %d exceeds headroom %d (raw headroom=%d, deposit=%d, requested=%d, priorOpen=%v, decision=%s, reason=%q)",
				env.MaxGrantMinorUnits, effectiveHeadroom, headroom, expectedDeposit, requested, priorOpen, env.Decision, env.Reason,
			)
		}

		// Never a negative grant.
		if env.MaxGrantMinorUnits < 0 {
			t.Fatalf("MaxGrantMinorUnits is negative: %d (in=%+v)", env.MaxGrantMinorUnits, in)
		}

		// A decline must carry a zero grant, and vice versa isn't
		// required (a zero *requested* amount is also declined with
		// zero), but Declined => 0 must always hold.
		if env.Decision == DecisionDeclined && env.MaxGrantMinorUnits != 0 {
			t.Fatalf("Decision=declined but MaxGrantMinorUnits=%d, want 0 (in=%+v)", env.MaxGrantMinorUnits, in)
		}

		// Never grant more than was actually requested.
		if requested > 0 && env.MaxGrantMinorUnits > requested {
			t.Fatalf("MaxGrantMinorUnits %d exceeds RequestedMinorUnits %d (in=%+v)", env.MaxGrantMinorUnits, requested, in)
		}
		if requested <= 0 && env.MaxGrantMinorUnits != 0 {
			t.Fatalf("non-positive request (%d) must grant 0, got %d (in=%+v)", requested, env.MaxGrantMinorUnits, in)
		}

		// The bridge cap recorded for audit purposes must never itself
		// exceed headroom either — it is defined as min(headroom,
		// fraction*deposit).
		if env.BridgeCapMinorUnits > effectiveHeadroom {
			t.Fatalf("BridgeCapMinorUnits %d exceeds headroom %d (in=%+v)", env.BridgeCapMinorUnits, effectiveHeadroom, in)
		}
		if env.BridgeCapMinorUnits < 0 {
			t.Fatalf("BridgeCapMinorUnits is negative: %d (in=%+v)", env.BridgeCapMinorUnits, in)
		}

		// Every decision must explain itself — this is what ends up in
		// the audit trail / LLM-visible rationale.
		if env.Reason == "" {
			t.Fatalf("empty Reason for decision %s (in=%+v)", env.Decision, in)
		}

		// A velocity-blocked tenant can never receive a grant,
		// regardless of how much headroom or deposit cap would
		// otherwise allow — this is rule 3 and must hold unconditionally.
		if priorOpen && env.MaxGrantMinorUnits != 0 {
			t.Fatalf("velocity cap should force MaxGrantMinorUnits=0, got %d (in=%+v)", env.MaxGrantMinorUnits, in)
		}
	})
}
