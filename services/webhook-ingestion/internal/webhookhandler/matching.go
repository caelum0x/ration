package webhookhandler

import ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"

// notYetReceived reports whether d is still outstanding (never marked
// received). Both matching tiers below must exclude already-received
// deposits from their candidate set entirely — see
// handleTransactionCreated's doc comment for why "excluded from the
// candidate set" and "merely deprioritized" are not the same thing
// here.
func notYetReceived(d *ledgerpb.Deposit) bool {
	return d.GetReceivedAtUnix() == 0
}

// matchDepositByReference returns the not-yet-received deposit in
// deposits whose own ExternalRef exactly equals reference AND whose
// expected amount equals amountMinorUnits, or nil if reference is
// empty or no deposit matches both.
//
// ExternalRef is set once, at RecordDeposit time (see
// services/gateway/internal/grpcserver's SimulateDeposit and
// services/cli/cmd/ration/seed.go), to the correlation key the tenant
// was told to put on their wire transfer. It is only ever overwritten
// later by MarkDepositReceived with the real transaction id — so for a
// still-outstanding deposit (the only kind this function considers) it
// is guaranteed to still hold that original correlation key, never the
// post-settlement value. Reference tags are assigned uniquely per
// deposit at creation, so at most one not-yet-received deposit can ever
// match on reference alone.
//
// The amount check matters because ledger-core's MarkDepositReceived
// has no "amount actually transferred" field at all (see
// MarkDepositReceivedRequest in proto/ledger.proto): it always credits
// the matched deposit's own pre-recorded expected amount, never
// anything from the webhook payload. Before this check, reference alone
// was treated as a strong-enough identification to skip amount
// validation entirely — so a real Airwallex transaction for ANY amount
// (including a transfer far smaller than expected, e.g. a shortfall or
// a reused/guessed correlation string) that happened to carry a
// deposit's reference would still mark that deposit fully received and
// credit the tenant's available balance for the FULL expected amount,
// regardless of how much money actually arrived. Reproduced live via
// this package's own test harness (fakeLedger + a real signed webhook
// payload): a $1.00 transaction carrying a $5,000.00 deposit's
// reference was credited as if the full $5,000.00 had arrived. Requiring
// amount equality here closes that gap — a reference match that doesn't
// also match amount is no longer trusted, and the caller falls through
// to matchDepositByAmountHeuristic below (same as if the reference
// hadn't matched at all).
func matchDepositByReference(deposits []*ledgerpb.Deposit, reference string, amountMinorUnits int64) *ledgerpb.Deposit {
	if reference == "" {
		return nil
	}
	for _, d := range deposits {
		if !notYetReceived(d) {
			continue
		}
		if d.GetExternalRef() == reference && d.GetExpectedAmount().GetMinorUnits() == amountMinorUnits {
			return d
		}
	}
	return nil
}

// matchDepositByAmountHeuristic returns the oldest not-yet-received
// deposit in deposits whose ExpectedAmount equals amountMinorUnits, or
// nil if none match. "Oldest" is the lowest ExpectedAtUnix — the same
// conservative, documented-as-a-heuristic tie-break this project has
// always used when a transaction carries no correlation key (or
// matches none), per handleTransactionCreated's doc comment on the
// residual misattribution risk this cannot close on its own.
func matchDepositByAmountHeuristic(deposits []*ledgerpb.Deposit, amountMinorUnits int64) *ledgerpb.Deposit {
	var best *ledgerpb.Deposit
	for _, d := range deposits {
		if !notYetReceived(d) {
			continue
		}
		if d.GetExpectedAmount().GetMinorUnits() != amountMinorUnits {
			continue
		}
		if best == nil || d.GetExpectedAtUnix() < best.GetExpectedAtUnix() {
			best = d
		}
	}
	return best
}
