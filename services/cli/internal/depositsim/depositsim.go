// Package depositsim computes the request parameters for
// `ration simulate-deposit`, kept separate from the cobra command so the
// late-vs-on-time timestamp math is unit-testable without a gRPC
// connection.
package depositsim

import "time"

// Params are the already-resolved values sent on
// gateway.SimulateDepositRequest.
type Params struct {
	ExpectedAtUnix int64
	MarkLate       bool
}

// Options configures how far in the future an on-time deposit is expected,
// and how far in the past a late deposit's expected_at is backdated.
type Options struct {
	// Now is injected for testability; callers should pass time.Now().
	Now time.Time
	// Late marks the deposit as already overdue.
	Late bool
	// ExpectedIn is how far in the future an on-time deposit is expected
	// to land. Ignored when Late is true.
	ExpectedIn time.Duration
	// LateBy is how far in the past a late deposit's expected_at_unix is
	// backdated, so the agent's next ListLateDeposits poll already picks
	// it up as late.
	LateBy time.Duration
}

// DefaultExpectedIn is used when the caller doesn't override how far out
// an on-time deposit is expected.
const DefaultExpectedIn = 48 * time.Hour

// DefaultLateBy is used when the caller doesn't override how far overdue
// a --late deposit is backdated.
const DefaultLateBy = 48 * time.Hour

// Resolve computes the expected_at_unix and mark_late fields to send on
// SimulateDepositRequest for the given options.
//
// A --late deposit gets expected_at_unix backdated into the past (not just
// mark_late=true) so the result is correct regardless of whether the
// gateway implementation re-derives lateness from mark_late or from
// comparing expected_at_unix against wall-clock time.
func Resolve(opts Options) Params {
	expectedIn := opts.ExpectedIn
	if expectedIn <= 0 {
		expectedIn = DefaultExpectedIn
	}
	lateBy := opts.LateBy
	if lateBy <= 0 {
		lateBy = DefaultLateBy
	}

	if opts.Late {
		return Params{
			ExpectedAtUnix: opts.Now.Add(-lateBy).Unix(),
			MarkLate:       true,
		}
	}
	return Params{
		ExpectedAtUnix: opts.Now.Add(expectedIn).Unix(),
		MarkLate:       false,
	}
}
