// Package policy implements the deterministic, auditable hard rules that
// gate every bridge-funding decision the agent makes. Nothing in this file
// calls the network, reads the clock, or depends on any other package in
// this service — every exported decision function is a pure function of
// its inputs so it can be exhaustively unit tested and so an LLM can never
// talk its way past it.
//
// See docs/AGENT-DESIGN.md "Hard rules (deterministic, never delegated to
// the LLM)" for the authoritative spec these functions implement:
//
//  1. Reserve floor: no grant may exceed the tenant's headroom above its
//     reserve floor.
//  2. Bridge cap: a single grant is capped at
//     min(headroom, expected_late_deposit_amount * BridgeMaxFraction).
//  3. Velocity cap: at most one open (unrepaid) bridge grant per tenant per
//     rolling velocity window.
//  4. Partial over decline: grant the largest amount that satisfies 1-3
//     rather than declining outright, unless that amount is below the
//     minimum-useful-grant floor, in which case decline with a reason.
package policy

import (
	"fmt"
	"os"
	"strconv"
)

// Decision is the outcome of evaluating the bridge-funding envelope.
type Decision string

const (
	// DecisionFullGrant means the full requested amount is approved.
	DecisionFullGrant Decision = "full_grant"
	// DecisionPartialGrant means less than requested is approved, per the
	// "partial over decline" rule.
	DecisionPartialGrant Decision = "partial_grant"
	// DecisionDeclined means no amount may be granted.
	DecisionDeclined Decision = "declined"
)

// Config holds the tunable policy parameters. These are read once at
// startup (see LoadConfigFromEnv) and then passed as plain data into the
// pure decision functions below — the functions themselves never read
// configuration from the environment.
type Config struct {
	// BridgeMaxFraction is the maximum fraction of an expected late deposit
	// that may be bridged in a single grant (rule 2). Default 0.6.
	BridgeMaxFraction float64
	// MinGrantMinorUnits is the minimum-useful-grant floor (rule 4). A
	// computed grant below this amount is declined instead of issued.
	// Default 500 ($5.00).
	MinGrantMinorUnits int64
	// VelocityWindowHours is the rolling window used by the velocity cap
	// (rule 3). Default 24.
	VelocityWindowHours int
}

// DefaultConfig returns the policy defaults documented in
// docs/AGENT-DESIGN.md and mirrored in .env.example.
func DefaultConfig() Config {
	return Config{
		BridgeMaxFraction:   0.6,
		MinGrantMinorUnits:  500,
		VelocityWindowHours: 24,
	}
}

// LoadConfigFromEnv builds a Config from environment variables, falling
// back to DefaultConfig for any variable that is unset or unparseable.
// This is the only function in the package that touches the environment;
// ComputeBridgeEnvelope and its helpers remain pure.
func LoadConfigFromEnv() Config {
	cfg := DefaultConfig()

	if v := os.Getenv("RATION_BRIDGE_MAX_FRACTION"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			cfg.BridgeMaxFraction = f
		}
	}
	if v := os.Getenv("RATION_BRIDGE_MIN_GRANT_MINOR_UNITS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.MinGrantMinorUnits = n
		}
	}
	if v := os.Getenv("RATION_BRIDGE_VELOCITY_WINDOW_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.VelocityWindowHours = n
		}
	}

	return cfg
}

// EnvelopeInput is everything ComputeBridgeEnvelope needs to know about a
// single bridge-funding decision. All money gathering (headroom lookups,
// velocity history) happens in internal/loop/runner.go; this struct is the
// pure boundary between "fetched state" and "policy math".
type EnvelopeInput struct {
	// HeadroomMinorUnits is the tenant's available balance above its
	// reserve floor, as returned by ledger-core's CheckReserveFloor RPC.
	// This is the authoritative, already-floor-aware number — rule 1 is
	// satisfied by never granting more than this.
	HeadroomMinorUnits int64
	// ExpectedDepositMinorUnits is the amount of the late deposit this
	// bridge grant is covering.
	ExpectedDepositMinorUnits int64
	// RequestedMinorUnits is the amount being asked for.
	RequestedMinorUnits int64
	// PriorGrantOpenWithinWindow is true when a bridge grant for this
	// tenant was made inside the rolling velocity window and has not yet
	// been repaid (its target deposit has not been marked received).
	PriorGrantOpenWithinWindow bool
}

// Envelope is the deterministic, auditable result of evaluating the hard
// rules. MaxGrantMinorUnits is the hard ceiling the LLM is told about and
// that the gateway independently re-validates — the LLM can decide how to
// phrase things and whether to tighten card controls, but it can never
// move this number.
type Envelope struct {
	HeadroomMinorUnits  int64
	BridgeCapMinorUnits int64
	MaxGrantMinorUnits  int64
	Decision            Decision
	Reason              string
}

// ComputeBridgeEnvelope evaluates rules 1-4 against in using cfg and
// returns the resulting envelope. It is a pure function: given the same
// cfg and in it always returns the same Envelope, with no I/O, no clock
// reads, and no randomness.
func ComputeBridgeEnvelope(cfg Config, in EnvelopeInput) Envelope {
	headroom := in.HeadroomMinorUnits
	if headroom < 0 {
		headroom = 0
	}

	bridgeCap := int64(float64(in.ExpectedDepositMinorUnits) * cfg.BridgeMaxFraction)
	if bridgeCap < 0 {
		bridgeCap = 0
	}

	bridgeCapAmount := minInt64(headroom, bridgeCap)

	env := Envelope{
		HeadroomMinorUnits:  headroom,
		BridgeCapMinorUnits: bridgeCapAmount,
	}

	if in.RequestedMinorUnits <= 0 {
		env.Decision = DecisionDeclined
		env.MaxGrantMinorUnits = 0
		env.Reason = "requested amount must be positive"
		return env
	}

	if headroom <= 0 {
		env.Decision = DecisionDeclined
		env.MaxGrantMinorUnits = 0
		env.Reason = "no headroom available above the reserve floor"
		return env
	}

	// Rule 3: velocity cap. Checked before computing a grantable amount —
	// an open prior grant blocks any new grant regardless of how much
	// headroom or bridge cap would otherwise allow.
	if in.PriorGrantOpenWithinWindow {
		env.Decision = DecisionDeclined
		env.MaxGrantMinorUnits = 0
		env.Reason = fmt.Sprintf(
			"velocity cap: a prior bridge grant within the last %dh has not yet been repaid",
			cfg.VelocityWindowHours,
		)
		return env
	}

	grantable := minInt64(in.RequestedMinorUnits, bridgeCapAmount)

	// Rule 4: partial over decline, unless below the minimum-useful-grant
	// floor.
	if grantable < cfg.MinGrantMinorUnits {
		env.Decision = DecisionDeclined
		env.MaxGrantMinorUnits = 0
		env.Reason = fmt.Sprintf(
			"computed grant of %d minor units is below the %d minor unit minimum-useful-grant floor",
			grantable, cfg.MinGrantMinorUnits,
		)
		return env
	}

	env.MaxGrantMinorUnits = grantable
	if grantable < in.RequestedMinorUnits {
		env.Decision = DecisionPartialGrant
		env.Reason = fmt.Sprintf(
			"partial grant: capped to %d minor units by reserve-floor headroom (%d) and the %.0f%% bridge cap (%d) against a %d request",
			grantable, headroom, cfg.BridgeMaxFraction*100, bridgeCapAmount, in.RequestedMinorUnits,
		)
		return env
	}

	env.Decision = DecisionFullGrant
	env.Reason = fmt.Sprintf(
		"full grant: %d minor units is within headroom (%d) and the bridge cap (%d)",
		grantable, headroom, bridgeCapAmount,
	)
	return env
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
