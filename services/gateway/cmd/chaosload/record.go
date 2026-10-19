package main

// opRecord is one JSON line in the chaos run's operation log (--log
// PATH). `load` appends one of these per logical ApplyEntry attempt
// (after however many transport-level retries it took to get a
// definitive answer, or after giving up at the deadline); `verify`
// reads them back and cross-checks each against ledger-core's own
// ListEntries/GetBalance state.
type opRecord struct {
	// IdempotencyKey is also used verbatim as the ApplyEntryRequest's
	// Reference field, specifically so verify can find this exact op
	// again via ListEntries — idempotency_key itself is not exposed over
	// the LedgerCore gRPC API (by design: it's an internal
	// exactly-once-application mechanism, not audit data), but
	// Reference is a free-form caller-supplied string that is.
	IdempotencyKey string `json:"idempotency_key"`

	// WorkerID/OpIndex identify which concurrent goroutine produced this
	// record and its sequence within that goroutine, for debugging.
	WorkerID int `json:"worker_id"`
	OpIndex  int `json:"op_index"`

	// Type is one of "CREDIT", "DEBIT", "HOLD", "RELEASE".
	Type             string `json:"type"`
	AmountMinorUnits int64  `json:"amount_minor_units"`

	// Outcome is one of:
	//   "applied"                — ledger-core confirmed the entry was written.
	//   "reserve_floor_violated" — ledger-core confirmed it rejected a DEBIT/HOLD (no WAL write).
	//   "insufficient_held"      — ledger-core confirmed it rejected a RELEASE (no WAL write).
	//   "unconfirmed"            — the test duration ran out while this op was still retrying
	//                              through a kill/restart gap; its true fate is unknown from the
	//                              client's point of view until verify checks the ledger directly.
	//   "error"                  — an unexpected, non-retryable error (a real bug, not chaos noise).
	Outcome string `json:"outcome"`

	// BalanceAfterAvailable/Held are only meaningful when Outcome ==
	// "applied"; they are what ledger-core's ApplyEntryResponse reported
	// at the moment of application.
	BalanceAfterAvailable int64 `json:"balance_after_available,omitempty"`
	BalanceAfterHeld      int64 `json:"balance_after_held,omitempty"`

	// Attempts is how many ApplyEntry RPCs (including transport
	// failures that triggered a retry of the same idempotency key) this
	// op took to reach its final Outcome.
	Attempts int `json:"attempts"`

	// ErrorDetail carries the error message when Outcome == "error".
	ErrorDetail string `json:"error_detail,omitempty"`
}
