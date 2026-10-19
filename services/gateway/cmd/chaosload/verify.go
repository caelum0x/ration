package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/ledgerclient"
)

// runVerify is the chaos test's actual oracle. It reads back the
// operation log `load` wrote, fetches the tenant's full entry history
// and balance from ledger-core (post however many SIGKILL/restart
// cycles the shell script put it through), and checks:
//
//  1. Every op the load generator got a definitive "applied" answer for
//     is present in ListEntries exactly once, with matching type and
//     amount — nothing lost.
//  2. Every op the load generator got a definitive decline
//     (reserve_floor_violated / insufficient_held) for did NOT make it
//     into ListEntries — nothing phantom-applied after being declined.
//  3. GetBalance reconciles exactly with summing every entry
//     ListEntries returned (order-independent arithmetic, since
//     concurrent entries can legitimately tie on the one-second
//     created_at_unix resolution) — ledger-core's own WAL-replay-into-
//     SQLite recovery path did not lose or double-count anything across
//     the crashes.
//  4. No DEBIT/HOLD entry's recorded balance_after_minor_units ever
//     reads below --reserve-floor — the actual invariant under test,
//     checked against the *real* historical balance at each entry's
//     true commit point, not a reconstructed replay order.
//
// Exits non-nil (process exit 1 via main.go) with every violation
// printed if any check fails.
func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	addr := fs.String("addr", "", "ledger-core gRPC address")
	tenant := fs.String("tenant", "", "tenant id to verify")
	logPath := fs.String("log", "", "path to the op log written by `load`")
	reserveFloor := fs.Int64("reserve-floor", 0, "the tenant's reserve floor, minor units")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" || *tenant == "" || *logPath == "" {
		return fmt.Errorf("--addr, --tenant and --log are all required")
	}

	records, err := readOpLog(*logPath)
	if err != nil {
		return fmt.Errorf("read op log: %w", err)
	}

	client, err := ledgerclient.Dial(*addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", *addr, err)
	}
	defer client.Close()
	client.Timeout = 10 * time.Second

	ctx := context.Background()
	entries, err := client.ListEntries(ctx, *tenant)
	if err != nil {
		return fmt.Errorf("ListEntries: %w", err)
	}
	balance, err := client.GetBalance(ctx, *tenant)
	if err != nil {
		return fmt.Errorf("GetBalance: %w", err)
	}

	byRef := make(map[string]*ledgerpb.LedgerEntry, len(entries))
	for _, e := range entries {
		if prior, dup := byRef[e.GetReference()]; dup {
			return fmt.Errorf(
				"VIOLATION: duplicate reference %q in ListEntries (entries %s and %s) — a write was double-applied",
				e.GetReference(), prior.GetId(), e.GetId(),
			)
		}
		byRef[e.GetReference()] = e
	}

	var violations []string
	var confirmedApplied, confirmedDeclined, unconfirmedCount int

	for _, rec := range records {
		switch rec.Outcome {
		case "applied":
			confirmedApplied++
			entry, ok := byRef[rec.IdempotencyKey]
			if !ok {
				violations = append(violations, fmt.Sprintf(
					"LOST ENTRY: op %s (%s %d) was confirmed applied by ledger-core but is missing from ListEntries",
					rec.IdempotencyKey, rec.Type, rec.AmountMinorUnits,
				))
				continue
			}
			if entryTypeNames[entry.GetType()] != rec.Type || entry.GetAmount().GetMinorUnits() != rec.AmountMinorUnits {
				violations = append(violations, fmt.Sprintf(
					"CORRUPTED ENTRY: op %s logged as (%s %d) but ListEntries has (%s %d)",
					rec.IdempotencyKey, rec.Type, rec.AmountMinorUnits,
					entryTypeNames[entry.GetType()], entry.GetAmount().GetMinorUnits(),
				))
			}
		case "reserve_floor_violated", "insufficient_held":
			confirmedDeclined++
			if _, ok := byRef[rec.IdempotencyKey]; ok {
				violations = append(violations, fmt.Sprintf(
					"GHOST ENTRY: op %s was confirmed DECLINED (%s) but it IS present in ListEntries — a declined write was applied anyway",
					rec.IdempotencyKey, rec.Outcome,
				))
			}
		case "unconfirmed":
			unconfirmedCount++
			// Its true fate is legitimately unknown to the client — it
			// may or may not have landed depending on exactly when
			// ledger-core died. Either is correct; we only note it.
		case "error":
			violations = append(violations, fmt.Sprintf(
				"LOAD GENERATOR ERROR: op %s got a non-retryable error: %s",
				rec.IdempotencyKey, rec.ErrorDetail,
			))
		}
	}

	summed := sumAvailableAndHeld(entries)
	if summed.available != balance.GetAvailableMinorUnits() {
		violations = append(violations, fmt.Sprintf(
			"RECONCILIATION MISMATCH: GetBalance.available=%d but summing all %d ListEntries gives %d — an entry was lost or double-applied across a WAL crash/recovery cycle",
			balance.GetAvailableMinorUnits(), len(entries), summed.available,
		))
	}
	if summed.held != balance.GetHeldMinorUnits() {
		violations = append(violations, fmt.Sprintf(
			"RECONCILIATION MISMATCH: GetBalance.held=%d but summing all %d ListEntries gives %d",
			balance.GetHeldMinorUnits(), len(entries), summed.held,
		))
	}

	for _, e := range entries {
		if e.GetType() == commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_DEBIT ||
			e.GetType() == commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD {
			if e.GetBalanceAfterMinorUnits() < *reserveFloor {
				violations = append(violations, fmt.Sprintf(
					"RESERVE FLOOR BREACHED: entry %s (%s %d, ref=%q) left available balance at %d, below the floor of %d",
					e.GetId(), entryTypeNames[e.GetType()], e.GetAmount().GetMinorUnits(), e.GetReference(),
					e.GetBalanceAfterMinorUnits(), *reserveFloor,
				))
			}
		}
	}

	fmt.Printf(
		"chaosload: verify — %d ledger entries, balance available=%d held=%d; log: %d applied-confirmed, %d declined-confirmed, %d unconfirmed\n",
		len(entries), balance.GetAvailableMinorUnits(), balance.GetHeldMinorUnits(),
		confirmedApplied, confirmedDeclined, unconfirmedCount,
	)

	if len(violations) > 0 {
		fmt.Printf("chaosload: %d VIOLATION(S) FOUND:\n", len(violations))
		for _, v := range violations {
			fmt.Println("  - " + v)
		}
		return fmt.Errorf("%d invariant violation(s)", len(violations))
	}
	return nil
}

func readOpLog(path string) ([]opRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []opRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec opRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("parse log line %q: %w", line, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

type shadowBalance struct {
	available int64
	held      int64
}

// sumAvailableAndHeld is a deliberately order-independent arithmetic
// reconciliation (see the doc comment on runVerify) — addition commutes,
// so it is a fully valid proof that GetBalance's live numbers are
// exactly explained by the entries ListEntries reports, regardless of
// the true commit order of concurrently-applied entries.
func sumAvailableAndHeld(entries []*ledgerpb.LedgerEntry) shadowBalance {
	var b shadowBalance
	for _, e := range entries {
		amt := e.GetAmount().GetMinorUnits()
		switch e.GetType() {
		case commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_CREDIT:
			b.available += amt
		case commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_DEBIT:
			b.available -= amt
		case commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD:
			b.available -= amt
			b.held += amt
		case commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE:
			b.available += amt
			b.held -= amt
		}
	}
	return b
}
