package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/ledgerclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// perAttemptTimeout bounds a single ApplyEntry RPC. Short enough that a
// half-dead connection (ledger-core mid-SIGKILL) fails fast into a
// retry rather than hanging; long enough not to false-positive under
// normal scheduling jitter.
const perAttemptTimeout = 2 * time.Second

// retryBackoff is the pause between retries of the same idempotency key
// after a transient (connection-level) failure — deliberately a bit
// longer than the chaos script's own kill-to-restart gap so most retries
// land after ledger-core has actually come back up rather than busy-
// looping against a still-dead process.
const retryBackoff = 150 * time.Millisecond

var entryTypeNames = map[commonpb.LedgerEntryType]string{
	commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_CREDIT:  "CREDIT",
	commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_DEBIT:   "DEBIT",
	commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD:    "HOLD",
	commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE: "RELEASE",
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	addr := fs.String("addr", "", "ledger-core gRPC address")
	tenant := fs.String("tenant", "", "tenant id to drive ApplyEntry load against")
	duration := fs.Duration("duration", 30*time.Second, "how long to generate load for")
	concurrency := fs.Int("concurrency", 8, "number of concurrent ApplyEntry callers")
	logPath := fs.String("log", "", "path to write one JSON line per op to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" || *tenant == "" || *logPath == "" {
		return fmt.Errorf("--addr, --tenant and --log are all required")
	}

	client, err := ledgerclient.Dial(*addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", *addr, err)
	}
	defer client.Close()
	client.Timeout = perAttemptTimeout

	logFile, err := os.Create(*logPath)
	if err != nil {
		return fmt.Errorf("create log %s: %w", *logPath, err)
	}
	defer logFile.Close()

	var logMu sync.Mutex
	enc := json.NewEncoder(logFile)
	writeRecord := func(rec opRecord) {
		logMu.Lock()
		defer logMu.Unlock()
		_ = enc.Encode(rec)
	}

	deadline := time.Now().Add(*duration)
	var applied, violated, insufficientHeld, unconfirmed, loadErrors atomic.Int64

	var wg sync.WaitGroup
	for workerID := 0; workerID < *concurrency; workerID++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			opIndex := 0
			for time.Now().Before(deadline) {
				rec := runOneOp(client, *tenant, workerID, opIndex, deadline)
				writeRecord(rec)
				switch rec.Outcome {
				case "applied":
					applied.Add(1)
				case "reserve_floor_violated":
					violated.Add(1)
				case "insufficient_held":
					insufficientHeld.Add(1)
				case "unconfirmed":
					unconfirmed.Add(1)
				case "error":
					loadErrors.Add(1)
				}
				opIndex++
			}
		}(workerID)
	}
	wg.Wait()

	fmt.Printf(
		"chaosload: load done — applied=%d reserve_floor_violated=%d insufficient_held=%d unconfirmed=%d errors=%d\n",
		applied.Load(), violated.Load(), insufficientHeld.Load(), unconfirmed.Load(), loadErrors.Load(),
	)
	if loadErrors.Load() > 0 {
		return fmt.Errorf("%d op(s) hit a non-retryable error — see %s", loadErrors.Load(), *logPath)
	}
	return nil
}

// randomOp picks a random entry type and amount for one logical chaos
// op. Amounts are sized comparably to a typical reserve floor (a few
// thousand minor units) so a healthy fraction of DEBIT/HOLD ops actually
// contend for headroom instead of trivially succeeding every time.
func randomOp() (commonpb.LedgerEntryType, int64) {
	kinds := []commonpb.LedgerEntryType{
		commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_CREDIT,
		commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_DEBIT,
		commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
		commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE,
	}
	kind := kinds[rand.IntN(len(kinds))]
	amount := int64(rand.IntN(8000)) + 1
	return kind, amount
}

// runOneOp drives exactly one logical ApplyEntry operation to a
// definitive outcome, retrying the SAME idempotency key across
// transport-level failures (i.e. exactly the window where the chaos
// script has SIGKILLed ledger-core and not yet restarted it) until
// either it gets a real answer or the overall load deadline passes.
// This is the behavior a real caller (the agent/gateway services) is
// specified to use, and it's what actually exercises "does a retried
// idempotent ApplyEntry across a crash recover correctly" rather than
// just "does ledger-core survive being killed while idle".
func runOneOp(client *ledgerclient.Client, tenantID string, workerID, opIndex int, deadline time.Time) opRecord {
	entryType, amount := randomOp()
	idempotencyKey := fmt.Sprintf("chaos-load-%d-%d", workerID, opIndex)

	rec := opRecord{
		IdempotencyKey:   idempotencyKey,
		WorkerID:         workerID,
		OpIndex:          opIndex,
		Type:             entryTypeNames[entryType],
		AmountMinorUnits: amount,
	}

	req := &ledgerpb.ApplyEntryRequest{
		TenantId: tenantID,
		Type:     entryType,
		Amount: &commonpb.Money{
			MinorUnits: amount,
			Currency:   "USD",
		},
		Reference:      idempotencyKey, // same value as the key, deliberately — see record.go
		IdempotencyKey: idempotencyKey,
		Memo:           "chaos load",
	}

	for {
		rec.Attempts++
		ctx, cancel := context.WithTimeout(context.Background(), perAttemptTimeout)
		resp, err := client.ApplyEntry(ctx, req)
		cancel()

		if err == nil {
			if resp.GetReserveFloorViolated() {
				rec.Outcome = "reserve_floor_violated"
			} else {
				rec.Outcome = "applied"
				rec.BalanceAfterAvailable = resp.GetBalanceAfter().GetAvailableMinorUnits()
				rec.BalanceAfterHeld = resp.GetBalanceAfter().GetHeldMinorUnits()
			}
			return rec
		}

		code := status.Code(err)
		if code == codes.FailedPrecondition {
			// RELEASE requested more than is currently held — a
			// legitimate business rejection, not a transport failure.
			// No WAL write happened; nothing to retry.
			rec.Outcome = "insufficient_held"
			return rec
		}
		if !isTransientGRPCCode(code) {
			rec.Outcome = "error"
			rec.ErrorDetail = err.Error()
			return rec
		}

		// Transient (connection-level) failure — almost certainly the
		// chaos script's SIGKILL window. Retry the exact same
		// idempotency key once ledger-core is back, unless the overall
		// load deadline has already passed.
		if time.Now().After(deadline) {
			rec.Outcome = "unconfirmed"
			return rec
		}
		time.Sleep(retryBackoff)
	}
}

func isTransientGRPCCode(code codes.Code) bool {
	switch code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Aborted, codes.Internal:
		return true
	default:
		return false
	}
}
